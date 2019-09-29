// © 2019 Steve McCoy. Licensed under the MIT License.

package main

import (
	"bufio"
	"encoding/gob"
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

	"github.com/zippoxer/RSS-Go"
)

var feeds = flag.String("feeds", "", "file containing a list of feeds")
var cert = flag.String("cert", "", "Certificate file")
var key = flag.String("key", "", "Private key for certificate")
var cache = flag.String("cache", "rss.gob", "File for storing feed results")
var freq = flag.Duration("freq", 4*time.Hour, "Duration between feed polls")

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

	toSave := make(chan []*rss.Feed)
	toShow := make(chan []*rss.Feed)
	go feedCache(toSave, toShow)
	go fetchFeeds(toSave, urls)

	http.Handle("/style/", http.StripPrefix("/style/", http.FileServer(http.Dir("style/"))))
	http.HandleFunc("/all", func(w http.ResponseWriter, r *http.Request) {
		listFeeds(w, time.Time{}, toShow)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			listFeeds(w, time.Now().AddDate(0, 0, -1), toShow)
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
	http.ListenAndServe(":http", nil)
}

func listFeeds(w io.Writer, since time.Time, fc <-chan []*rss.Feed) {
	feeds := <-fc
	entries := filterEntries(feeds, since)
	listPage.Execute(w, entries)
}

func feedCache(toSave <-chan []*rss.Feed, toShow chan<- []*rss.Feed) {
	var feedz []*rss.Feed
	for {
		select {
		case toShow <- feedz:
			// I just sent it.
		case feedz = <-toSave:
			saveFeeds(feedz)
		}
	}
}

func saveFeeds(feeds []*rss.Feed) {
	f, err := os.Create(*cache)
	maybeDie(err)
	defer f.Close()

	enc := gob.NewEncoder(f)
	enc.Encode(feeds)
}

func fetchFeeds(db chan<- []*rss.Feed, urls []string) {
	f, err := os.Open(*cache)
	if err != nil {
		fetch(db, urls)
	} else {
		var feeds []*rss.Feed
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

func fetch(db chan<- []*rss.Feed, urls []string) {
	n := 0
	feeds := []*rss.Feed{}
	errs := []error{}
	fc := make(chan *rss.Feed)
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
			feeds = append(feeds, f)
		case e := <-ec:
			errs = append(errs, e)
		}
	}

	db <- feeds

	for _, e := range errs {
		log.Printf("Problem: %v\n", e)
	}
}

func getFeed(s string, fc chan *rss.Feed, ec chan error) {
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

	feed, err := rss.Get(resp.Body)
	if err != nil {
		ec <- errors.New(s + ": " + err.Error())
		return
	}

	fc <- feed
}

func maybeDie(err error) {
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
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

func filterEntries(feeds []*rss.Feed, begin time.Time) []Entry {
	var filtered []Entry
	for _, f := range feeds {
		for _, i := range f.Items {
			if i.When.After(begin) {
				filtered = append(filtered, Entry{
					FeedName: f.Title,
					FeedURL:  f.Link,
					Title:    i.Title,
					URL:      i.Link,
					When:     i.When,
				})
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].When.After(filtered[j].When)
	})
	return filtered
}

var listPage = template.Must(template.New("listing").Parse(listPageTemplate))

var listPageTemplate = `<!DOCTYPE html>
<html>
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">

	<link rel="icon" href="style/favicon.png">
	<link rel="stylesheet" href="style/stk5.css">

	<title>WEBRSS</title>
</head>

<body>
{{if .}}
	<ul>
{{range .}}
	<li>
		<h1><a href="{{.URL}}">{{.Title}}</a></h1>
		<p class="details">{{.When.Format "Mon 02/01/2006"}}, <a href="{{.FeedURL}}">{{.FeedName}}</a></p>
	</li>
{{end}}
	</ul>
{{else}}
	<p>Nothing to see, yet. ⏳</p>
{{end}}
</body>
</html>
`