// © 2021 Steve McCoy. Licensed under the MIT License.

package main

import (
	"bufio"
	"encoding/gob"
	"encoding/xml"
	"errors"
	"flag"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"
)

var feeds = flag.String("feeds", "", "file containing a list of feeds")
var cert = flag.String("cert", "", "Certificate file")
var key = flag.String("key", "", "Private key for certificate")
var cache = flag.String("cache", "rss.gob", "File for storing feed results")
var freq = flag.Duration("freq", 1*time.Hour, "Duration between feed polls")
var httpAddr = flag.String("http", ":http", "HTTP listen address (in typical Dial fashion)")

func main() {
	flag.Parse()

	if flag.NArg() == 0 && *feeds == "" {
		os.Stderr.WriteString("I need the feed URL.\n")
		os.Exit(1)
	}

	var urls []string
	if flag.NArg() > 0 {
		urls = append(urls, flag.Args()...)
	}

	if *feeds != "" {
		f, err := os.Open(*feeds)
		maybeDie(err)

		in := bufio.NewScanner(f)
		for in.Scan() {
			urls = append(urls, in.Text())
		}
		f.Close()
		maybeDie(in.Err())
	}

	toSave := make(chan []Entry)
	toShow := make(chan []Entry)
	go feedCache(toSave, toShow)
	go fetchFeeds(toSave, urls)

	http.Handle("/style/", http.StripPrefix("/style/", http.FileServer(http.Dir("style/"))))
	http.HandleFunc("/day", func(w http.ResponseWriter, r *http.Request) {
		showDaily(w, time.Now().UTC(), toShow)
	})
	http.HandleFunc("/yesterday", func(w http.ResponseWriter, r *http.Request) {
		t := time.Now().UTC().AddDate(0, 0, -1)
		showDaily(w, t, toShow)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			showDaily(w, time.Now().UTC(), toShow)
		} else {
			http.NotFound(w, r)
		}
	})
	if *cert != "" && *key != "" {
		go func() {
			err := http.ListenAndServeTLS(":https", *cert, *key, nil)
			log.Println(err)
		}()
	}
	http.ListenAndServe(*httpAddr, nil)
}

func showDaily(w io.Writer, day time.Time, fc <-chan []Entry) {
	feeds := <-fc
	entries := filterEntries(feeds, day, day.AddDate(0, 0, 1))

	sites := map[string][]Entry{}
	for i := range entries {
		sites[entries[i].FeedName] = append(sites[entries[i].FeedName], entries[i])
	}

	var d Daily
	for s := range sites {
		if len(sites[s]) == 1 {
			d.Singles = append(d.Singles, sites[s][0])
		} else {
			d.Sites = append(d.Sites, Site{s, sites[s]})
		}
	}
	sort.Slice(d.Sites, func(i, j int) bool {
		return d.Sites[i].Name < d.Sites[j].Name
	})

	dailyPage.Execute(w, d)
}

func feedCache(toSave <-chan []Entry, toShow chan<- []Entry) {
	var feedz []Entry
	for {
		select {
		case toShow <- feedz:
			// I just sent it.
		case feedz = <-toSave:
			saveFeeds(feedz)
		}
	}
}

func saveFeeds(feeds []Entry) {
	f, err := os.Create(*cache)
	maybeDie(err)
	defer f.Close()

	enc := gob.NewEncoder(f)
	enc.Encode(feeds)
}

func fetchFeeds(db chan<- []Entry, urls []string) {
	f, err := os.Open(*cache)
	if err != nil {
		fetch(db, urls)
	} else {
		var feeds []Entry
		dec := gob.NewDecoder(f)
		err := dec.Decode(&feeds)
		f.Close()
		maybeDie(err)
		db <- feeds
	}

	tt := time.Tick(*freq)
	for _ = range tt {
		fetch(db, urls)
	}
}

func fetch(db chan<- []Entry, urls []string) {
	log.Printf("It's time to fetch %d feeds.", len(urls))
	n := 0
	var feeds []Entry
	errs := []error{}
	fc := make(chan []Entry)
	ec := make(chan error)

	for _, u := range urls {
		if len(u) == 0 {
			continue
		}

		n++
		go getFeed(u, fc, ec)
	}

	for i := 0; i < n; i++ {
		select {
		case f := <-fc:
			feeds = append(feeds, f...)
		case e := <-ec:
			errs = append(errs, e)
		}
	}

	db <- feeds

	for _, e := range errs {
		log.Printf("Problem: %v\n", e)
	}
	log.Println("Done fetching.")
}

func getFeed(s string, fc chan []Entry, ec chan error) {
	url, err := url.Parse(s)
	if err != nil {
		ec <- errors.New(s + ": " + err.Error())
		return
	}

	resp, err := http.Get(url.String())
	if err != nil {
		ec <- errors.New(s + ": " + err.Error())
		return
	}
	defer resp.Body.Close()

	entries, err := tryParse(resp.Body)
	if err != nil {
		ec <- errors.New(s + ": " + err.Error())
		return
	}
	fc <- entries
}

func maybeDie(err error) {
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

type Feed struct {
	atom *Atom1
	rss  *Rss2
}

func (f *Feed) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local == "rss" {

		return d.DecodeElement(&f.rss, &start)
	}
	return d.DecodeElement(&f.atom, &start)
}

func tryParse(r io.Reader) ([]Entry, error) {
	var feed Feed
	d := xml.NewDecoder(r)
	err := d.Decode(&feed)
	if err != nil {
		return nil, err
	}
	var entries []Entry

	if feed.atom != nil {
		for _, i := range feed.atom.Items {
			when, err := time.Parse(time.RFC3339, i.When)
			if err != nil {
				log.Printf("Time parse error for %q: atom gives %v\n", i.Title, err)
			}
			entries = append(entries, Entry{
				FeedName: feed.atom.Title,
				FeedURL:  feed.atom.Link.URL,
				Title:    i.Title,
				URL:      i.Link.URL,
				When:     when,
			})
		}
	} else {
		for _, i := range feed.rss.Channel.Items {
			when, err := parseRssTimes(i.When)
			if err != nil {
				log.Printf("Time parse error for %q: rss gives %v\n", i.Title, err)
			}
			entries = append(entries, Entry{
				FeedName: feed.rss.Channel.Title,
				FeedURL:  feed.rss.Channel.Link,
				Title:    i.Title,
				URL:      i.Link,
				When:     when,
			})
		}
	}
	return entries, nil
}

func parseRssTimes(ts string) (time.Time, error) {
	fmts := []string{time.RFC822, time.RFC822Z, time.RFC1123, time.RFC1123Z}
	var t time.Time
	var err error
	for _, f := range fmts {
		t, err = time.Parse(f, ts)
		if err == nil {
			return t, nil
		}
	}
	return t, err
}

type Atom1 struct {
	Title string `xml:"title"`
	Link  struct {
		URL string `xml:"href,attr"`
	} `xml:"link"`

	Items []struct {
		Title string `xml:"title"`
		Link  struct {
			URL string `xml:"href,attr"`
		} `xml:"link"`
		When string `xml:"updated"`
	} `xml:"entry"`
}

type Rss2 struct {
	Channel struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`

		Items []struct {
			Title string `xml:"title"`
			Link  string `xml:"link"`
			When  string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

type ListingPage struct {
	Feeds []Entry
	Begin time.Time
}

type Entry struct {
	FeedName string
	FeedURL  string
	Title    string
	URL      string
	When     time.Time
}

func filterEntries(feeds []Entry, begin, end time.Time) []Entry {
	var filtered []Entry
	for _, i := range feeds {
		if i.When.After(begin) && (end.IsZero() || end.After(i.When)) {
			filtered = append(filtered, i)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].When.After(filtered[j].When)
	})
	return filtered
}

var dailyPage = template.Must(template.New("daily").Parse(dailyPageTemplate))

type Daily struct {
	Sites []Site
	Singles []Entry
}

type Site struct {
	Name string
	Entries []Entry
}

var dailyPageTemplate = `<!DOCTYPE html>
<html>
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">

	<link rel="icon" href="style/favicon.png">
	<link rel="stylesheet" href="style/feed.css">

	<title>WEBRSS Today</title>
</head>

<body>
{{if .Singles}}
		<div class="card">
			<h1>★ Singles ★</h1>
			<ul>
{{range .Singles}}
				<li class="card-item"><a href="{{.URL}}">{{.Title}}</a><span class="details"> (<a href="{{.FeedURL}}">{{.FeedName}}</a>)</span></li>
{{end}}
			</ul>
		</div>
{{end}}
{{if .Sites}}
	<ul>
{{range .Sites}}
		<li class="card">
			<h1>{{.Name}}</h1>
			<ul>
{{range .Entries}}
				<li class="card-item"><a href="{{.URL}}">{{.Title}}</a></li>
{{end}}
			</ul>
		</li>
{{end}}
	</ul>
{{end}}
</body>
</html>
`
