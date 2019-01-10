package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"
)

const (
	topStories  = "https://hacker-news.firebaseio.com/v0/topstories.json"
	bestStories = "https://hacker-news.firebaseio.com/v0/beststories.json"
	storyLink   = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnPostLink  = "https://news.ycombinator.com/item?id=%d"
)

type itemList []int

type unixTime int64

type item struct {
	Time  unixTime `json:"time"`
	Title string   `json:"title"`
	URL   string   `json:"url"`
}

type stories struct {
	sync.Mutex
	list map[int]item
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	stop := make(chan os.Signal, 1)

	signal.Notify(stop, os.Interrupt)

	tmpl := template.New("index.html")
	tmpl, err := tmpl.ParseFiles("./index.html")
	if err != nil {
		panic(err)
	}
	st := stories{
		list: make(map[int]item),
	}

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	storiesURLs := []string{topStories, bestStories}
	incomingItems := make(chan itemList)
	defer close(incomingItems)

	storyFetcher := func(ctx context.Context) {
		for {
			select {
			case <-ticker.C:
				var wg sync.WaitGroup
				for _, storiesURL := range storiesURLs {
					wg.Add(1)
					go func(storiesURL string) {
						defer wg.Done()
						resp, err := http.Get(storiesURL)
						if err != nil {
							log.Println(err)
							return
						}

						defer resp.Body.Close()

						decoder := json.NewDecoder(resp.Body)
						var items itemList
						err = decoder.Decode(&items)
						if err != nil {
							log.Println(err)
							return
						}

						// send items
						incomingItems <- items
					}(storiesURL)
				}
				wg.Wait()
			case <-ctx.Done():
				return
			}
		}
	}

	storyLister := func(ctx context.Context) {
		for {
			select {
			case items := <-incomingItems:
				for _, itemID := range items {
					func() {
						if _, ok := st.list[itemID]; ok {
							return
						}

						item, err := fetchItem(itemID)
						if err != nil {
							log.Println(err)
							return
						}
						if time.Since(time.Unix(int64(item.Time), 0)).Hours() > 8 {
							return
						}

						st.Lock()
						defer st.Unlock()
						st.list[itemID] = item
					}()
				}
			case <-ctx.Done():
				return
			}
		}
	}

	ticker2 := time.NewTicker(5 * time.Minute)
	defer ticker2.Stop()

	storyRemover := func(ctx context.Context) {
		for {
			select {
			case <-ticker2.C:
				for id, it := range st.list {
					if time.Since(time.Unix(int64(it.Time), 0)).Hours() > 8 {
						st.Lock()
						delete(st.list, id)
						st.Unlock()
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}

	go storyFetcher(ctx)
	go storyLister(ctx)
	go storyRemover(ctx)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		err = tmpl.Execute(w, st.list)
		if err != nil {
			log.Println(err)
		}
		return
	})

	log.Fatal(http.ListenAndServe(":8080", nil))

	<-stop
	cancel()
}

func fetchItem(itemID int) (i item, err error) {
	resp, err := http.Get(fmt.Sprintf(storyLink, itemID))
	if err != nil {
		return
	}

	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&i)
	if err != nil {
		return
	}

	return i, nil
}
