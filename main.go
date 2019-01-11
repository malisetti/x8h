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
	hnPollTime           = 5 * time.Minute
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
	defer close(errCh)

	st := stories{
		list: make(map[int]item),
	}

	storiesURLs := []string{topStories}
	incomingItems := make(chan itemList)
	defer close(incomingItems)

	stop := make(chan os.Signal, 1)

	signal.Notify(stop, os.Interrupt, os.Kill)

	ctx, cancel := context.WithCancel(context.Background())

	const trackingLimit = 2000
	visited := make(map[int]struct{})
	lm := newLM(visited, trackingLimit)

	tickerTicker := func(ctx context.Context, tickers []*time.Ticker, calls []func(ctx context.Context) error) error {
		var eg errgroup.Group
		for i, tckr := range tickers {
			i := i
			tckr := tckr
			eg.Go(func() error {
				for range tckr.C {
					err := calls[i](ctx)
					if err != nil {
						return err
					}
				}
				return nil
			})
		}

		return eg.Wait()
	}

	topStoriesFetcher := func(limit int) error {
		var eg errgroup.Group
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

	storyRemover := func() error {
		for id, it := range st.list {
			if time.Since(time.Unix(int64(it.Added), 0)).Seconds() > 8*60*60 {
				st.Lock()
				delete(st.list, id)
				st.Unlock()
			}
		}
		return nil
	}

	fiveMinTicker := time.NewTicker(hnPollTime)
	defer fiveMinTicker.Stop()

	listCounter := func() error {
		st.Lock()
		defer st.Unlock()
		log.Println(st.list)
		log.Println(visited)
		return nil
	}

	tickers := []*time.Ticker{fiveMinTicker}
	calls := []func(ctx context.Context) error{
		func(ctx context.Context) error {
			var eg errgroup.Group
			eg.Go(func() error {
				log.Println("starting top stories fetcher")
				return topStoriesFetcher(frontPageNumArticles)
			})
			eg.Go(func() error {
				log.Println("starting story remover")
				return storyRemover()
			})
			eg.Go(func() error {
				log.Println("starting list counter")
				return listCounter()
			})
			return eg.Wait()
		},
	}

	errLogger := func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case err := <-errCh:
				log.Println(err)
			}
		}
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		st.Lock()
		defer st.Unlock()
		err = tmpl.Execute(w, st.list)
		if err != nil {
			errCh <- err
		}
		return
	})

	log.Println("START")
	log.Println("starting the app")
	var eg errgroup.Group
	eg.Go(func() error {
		log.Println("starting error logger")
		return errLogger(ctx)
	})
	eg.Go(func() error {
		log.Println("starting top stories fetcher")
		return topStoriesFetcher(frontPageNumArticles)
	})
	eg.Go(func() error {
		time.Sleep(1 * time.Minute)
		return listCounter()
	})
	eg.Go(func() error {
		log.Println("starting story lister")
		return storyLister(ctx)
	})
	eg.Go(func() error {
		log.Println("starting ticker ticker")
		return tickerTicker(ctx, tickers, calls)
	})
	eg.Go(func() error {
		log.Println("starting http server on port 8080")
		return http.ListenAndServe(":8080", nil)
	})
	eg.Go(func() error {
		sig := <-stop
		return fmt.Errorf("interrupted with signal %s, aborting", sig.String())
	})

	err = eg.Wait()
	if err != nil {
		log.Println(err)
	}

	cancel()
	log.Println("END")
}
