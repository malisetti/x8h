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

	"golang.org/x/sync/errgroup"
)

const (
	topStories           = "https://hacker-news.firebaseio.com/v0/topstories.json"
	bestStories          = "https://hacker-news.firebaseio.com/v0/beststories.json"
	storyLink            = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnPostLink           = "https://news.ycombinator.com/item?id=%d"
	frontPageNumArticles = 30
	hnPollTime           = 1 * time.Minute
	port                 = 8080
)

type limitMap struct {
	sync.Mutex
	keys []int
	m    map[int]struct{}
	l    int
}

func (lm *limitMap) insert(k int) {
	lm.Lock()
	defer lm.Unlock()

	lm.m[k] = struct{}{}
	lm.keys = append(lm.keys, k)

	if len(lm.keys) >= lm.l {
		delete(lm.m, lm.keys[0])
		lm.keys = lm.keys[1:]
	}
}

func (lm *limitMap) has(k int) bool {
	lm.Lock()
	defer lm.Unlock()

	_, ok := lm.m[k]

	return ok
}

func newLM(m map[int]struct{}, l int) *limitMap {
	keys := []int{}
	for k, _ := range m {
		keys = append(keys, k)
	}
	return &limitMap{
		keys: keys,
		m:    m,
		l:    l,
	}
}

type itemList []int

type unixTime int64

type item struct {
	Title string   `json:"title"`
	URL   string   `json:"url"`
	Added unixTime `json:"-"`
}

type stories struct {
	sync.Mutex
	list map[int]item
}

func main() {
	tmpl := template.New("index.html")
	tmpl, err := tmpl.ParseFiles("./index.html")
	if err != nil {
		panic(err)
	}

	errCh := make(chan error)

	st := stories{
		list: make(map[int]item),
	}

	storiesURLs := []string{topStories}
	incomingItems := make(chan itemList)

	const trackingLimit = 2000
	visited := make(map[int]struct{})
	lm := newLM(visited, trackingLimit)

	topStoriesFetcher := func(ctx context.Context, limit int) error {
		eg, ctx := errgroup.WithContext(ctx)
		for _, storiesURL := range storiesURLs {
			storiesURL := storiesURL
			eg.Go(func() error {
				fetchStories := func(limit int) ([]int, error) {
					resp, err := http.Get(storiesURL)
					if err != nil {
						return nil, err
					}

					defer resp.Body.Close()

					decoder := json.NewDecoder(resp.Body)
					var items itemList
					err = decoder.Decode(&items)
					if err != nil {
						return nil, err
					}
					if len(items) < limit {
						limit = len(items)
					}
					return items[:limit], nil
				}

				// send items
				items, err := fetchStories(limit)
				if err != nil {
					return err
				}
				incomingItems <- items
				return nil
			})
		}

		return eg.Wait()
	}

	fetchItem := func(itemID int) (i item, err error) {
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

	storyLister := func(ctx context.Context) error {
		for {
			select {
			case items := <-incomingItems:
				for _, itemID := range items {
					select {
					case <-ctx.Done():
						return nil
					default:
						func() {
							if _, ok := st.list[itemID]; ok {
								return
							}

							if lm.has(itemID) {
								return
							}

							lm.insert(itemID)

							item, err := fetchItem(itemID)
							if err != nil {
								errCh <- err
								return
							}
							item.Added = unixTime(time.Now().Unix())

							st.Lock()
							defer st.Unlock()
							st.list[itemID] = item
						}()
					}
				}
			case <-ctx.Done():
				return nil
			}
		}
	}

	storyRemover := func(ctx context.Context) error {
		st.Lock()
		defer st.Unlock()
		for id, it := range st.list {
			if ctx.Err() != nil {
				return nil
			}
			if time.Since(time.Unix(int64(it.Added), 0)).Seconds() > 8*60*60 {
				delete(st.list, id)
			}
		}
		return nil
	}

	listCounter := func() error {
		st.Lock()
		defer st.Unlock()
		var ids []int
		for id := range st.list {
			ids = append(ids, id)
		}
		log.Println(ids)
		ids = ids[:0]
		for id := range visited {
			ids = append(ids, id)
		}
		log.Println(ids)
		return nil
	}

	errLogger := func(ctx context.Context) error {
		for ctx.Err() == nil {
			err := <-errCh
			if err != nil {
				log.Println(err)
			}
		}
		return nil
	}

	visitCounterCh := make(chan int)

	var visitCount int
	visitCounter := func() error {
		for c := range visitCounterCh {
			visitCount += c
		}
		return nil
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			visitCounterCh <- 1
		}()

		st.Lock()
		defer st.Unlock()
		data := make(map[string]interface{})
		data["Data"] = st.list
		data["VisitorNumber"] = visitCount
		err = tmpl.Execute(w, data)
		if err != nil {
			errCh <- err
		}
		return
	})

	log.Println("START")
	log.Println("starting the app")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, os.Kill)

	fiveMinTicker := time.NewTicker(hnPollTime)

	appCtx, cancel := context.WithCancel(context.Background())

	go func() {
		for range fiveMinTicker.C {
			log.Println("starting ticker ticker")
			eg, ctxi := errgroup.WithContext(appCtx)
			eg.Go(func() error {
				log.Println("starting top stories fetcher")
				return topStoriesFetcher(ctxi, frontPageNumArticles)
			})
			eg.Go(func() error {
				log.Println("starting story remover")
				return storyRemover(ctxi)
			})
			eg.Go(func() error {
				log.Println("starting list counter")
				return listCounter()
			})
			err := eg.Wait()
			if err != nil {
				errCh <- err
			}
		}
	}()

	eg, ctx := errgroup.WithContext(appCtx)

	eg.Go(func() error {
		log.Println("starting error logger")
		return errLogger(ctx)
	})
	eg.Go(func() error {
		log.Println("starting top stories fetcher")
		return topStoriesFetcher(ctx, frontPageNumArticles)
	})
	eg.Go(func() error {
		log.Println("starting story lister")
		return storyLister(ctx)
	})

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port)}

	eg.Go(func() error {
		log.Println("starting http server on port 8080")

		return srv.ListenAndServe()
	})
	eg.Go(func() error {
		defer func() {
			cancel()
			fiveMinTicker.Stop()
			close(incomingItems)
			close(visitCounterCh)
			close(errCh)
		}()
		sig := <-stop
		log.Printf("interrupted with signal %s, aborting\n", sig.String())
		return srv.Shutdown(ctx)
	})
	eg.Go(visitCounter)

	err = eg.Wait()
	if err != nil {
		log.Println(err)
	}

	cancel()
	log.Println("END")
}
