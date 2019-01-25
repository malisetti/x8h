package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/middleware/stdlib"
	sim "github.com/ulule/limiter/v3/drivers/store/memory"

	"golang.org/x/sync/errgroup"
)

const (
	topStories           = "https://hacker-news.firebaseio.com/v0/topstories.json"
	bestStories          = "https://hacker-news.firebaseio.com/v0/beststories.json"
	storyLink            = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnPostLink           = "https://news.ycombinator.com/item?id=%d"
	frontPageNumArticles = 30
	hnPollTime           = 1 * time.Minute
	defaultPort          = 8080

	rateLimit          = "5-M"
	humanTrackingLimit = 300
)

type changeAction string

const (
	changeAdd    changeAction = "added"
	changeRemove              = "removed"
)

type logLevel string

const (
	logInfo    logLevel = "info"
	logWarning          = "warning"
	logDebug            = "debug"
	logFatal            = "fatal"
)

type appErr struct {
	err   error
	level logLevel
}

func (ae appErr) Error() string {
	return fmt.Sprintf("occured: %v with level: %s", ae.err, ae.level)
}

type itemList []int

type unixTime int64

type item struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Deleted bool   `json:"deleted"`
	Dead    bool   `json:"dead"`

	Added  unixTime `json:"-"`
	Domain string   `json:"-"`
}

type change struct {
	Action changeAction
	Item   item
}

func (c change) String() string {
	return fmt.Sprintf("%s : %d", c.Action, c.Item.ID)
}

type stories struct {
	sync.Mutex
	list map[int]item
}

type adder struct {
	sync.Mutex
	count int
}

var version string

func main() {
	var port int
	// use PORT env else port
	envPort := os.Getenv("PORT")
	if envPort == "" {
		port = defaultPort
	} else {
		var err error
		port, err = strconv.Atoi(envPort)
		if err != nil {
			panic(err)
		}
	}

	templateFile := os.Getenv("TMPL_PATH")
	if templateFile == "" {
		templateFile = "./index.html"
	}

	tmpl := template.New("index.html")
	tmpl, err := tmpl.ParseFiles(templateFile)
	if err != nil {
		panic(err)
	}

	store := sim.NewStore()
	// Define a limit rate to 5 requests per minute.
	rate, err := limiter.NewRateFromFormatted(rateLimit)
	if err != nil {
		panic(err)
	}

	middleware := stdlib.NewMiddleware(limiter.New(store, rate, limiter.WithTrustForwardHeader(true)))

	appCtx, cancel := context.WithCancel(context.Background())

	errCh := make(chan appErr)
	changeCh := make(chan change)

	st := stories{
		list: make(map[int]item),
	}

	incomingItems := make(chan itemList)

	r := strings.NewReplacer("http://", "", "https://", "", "www.", "", "www2.", "", "www3.", "")
	urlToDomain := func(link string) (string, error) {
		u, err := url.Parse(link)
		if err != nil {
			return "", err
		}
		parts := strings.Split(u.Hostname(), ".")
		if len(parts) >= 2 {
			domain := parts[len(parts)-2] + "." + parts[len(parts)-1]
			return domain, nil
		}

		return r.Replace(u.Hostname()), nil
	}

	fetchTopStories := func(ctx context.Context, limit int) ([]int, error) {
		req, err := http.NewRequest(http.MethodGet, topStories, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
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

	topStoriesFetcher := func(ctx context.Context, limit int) error {
		// send items
		items, err := fetchTopStories(ctx, limit)
		if err != nil {
			return err
		}
		if appCtx.Err() == nil {
			incomingItems <- items
		}
		return nil
	}

	fetchItem := func(ctx context.Context, itemID int) (i item, err error) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(storyLink, itemID), nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
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
		lookup := make(map[int]struct{})
		var idHolder []int
		for {
			select {
			case items := <-incomingItems:
				for _, itemID := range items {
					select {
					case <-ctx.Done():
						return nil
					default:
						func() {
							if _, ok := lookup[itemID]; ok {
								return
							}
							item, err := fetchItem(ctx, itemID)
							if err != nil {
								log.Println(err)
								return
							}

							idHolder = append(idHolder, itemID)
							lookup[itemID] = struct{}{}
							if len(lookup) >= humanTrackingLimit {
								delete(lookup, idHolder[0])
								idHolder = idHolder[1:]
							}

							item.Added = unixTime(time.Now().Unix())
							domain, err := urlToDomain(item.URL)
							if err == nil {
								item.Domain = domain
							} else {
								log.Println(err)
							}

							st.Lock()
							defer st.Unlock()
							st.list[itemID] = item
							// send this to added chan
							changeCh <- change{
								Action: changeAdd,
								Item:   item,
							}
						}()
					}
				}
			case <-ctx.Done():
				return nil
			}
		}
	}

	storyRemover := func(ctx context.Context) error {
		topItems, err := fetchTopStories(ctx, frontPageNumArticles)
		if err != nil {
			return err
		}

		st.Lock()
		defer st.Unlock()
		for id, it := range st.list {
			stillAtTop := false
			for _, tid := range topItems {
				if tid == id {
					stillAtTop = true
					break
				}
			}
			if stillAtTop {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			if time.Since(time.Unix(int64(it.Added), 0)).Seconds() > 8*60*60 {
				// send these to removed chan
				changeCh <- change{
					Action: changeRemove,
					Item:   st.list[id],
				}
				delete(st.list, id)
			}
		}
		return nil
	}

	changeLogger := func() error {
		for c := range changeCh {
			log.Println(c)
		}
		return nil
	}

	errLogger := func() error {
		for {
			select {
			case err := <-errCh:
				if err.level == logFatal {
					return err
				}
				log.Println(err)

			case <-appCtx.Done():
				return nil
			}
		}
	}

	var visitCount adder

	http.Handle("/", middleware.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.Lock()
		defer st.Unlock()

		data := make(map[string]interface{})
		data["Data"] = st.list

		visitCount.Lock()
		defer visitCount.Unlock()

		visitCount.count++

		data["Visits"] = visitCount.count
		data["Version"] = version

		err = tmpl.Execute(w, data)
		if err != nil {
			errCh <- appErr{
				err:   err,
				level: logFatal,
			}
		}
		return
	})))

	log.Println("START")
	log.Println("starting the app")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, os.Kill)

	intervalTicker := time.NewTicker(hnPollTime)

	go func() {
		for range intervalTicker.C {
			log.Println("starting ticker ticker")
			eg, ctx := errgroup.WithContext(appCtx)
			eg.Go(func() error {
				log.Println("starting top stories fetcher")
				return topStoriesFetcher(ctx, frontPageNumArticles)
			})
			eg.Go(func() error {
				log.Println("starting story remover")
				return storyRemover(ctx)
			})
			err := eg.Wait()
			if err != nil {
				errCh <- appErr{
					err:   err,
					level: logFatal,
				}
			}
		}
	}()

	eg, ctx := errgroup.WithContext(appCtx)

	eg.Go(func() error {
		log.Println("starting error logger")
		return errLogger()
	})
	eg.Go(func() error {
		log.Println("starting change logger")
		return changeLogger()
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

	errors := make(chan error)
	defer close(errors)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer log.Println("done with signal")
		defer wg.Done()
		for {
			select {
			case <-appCtx.Done():
				return
			case sig := <-stop:
				errors <- appErr{
					err:   fmt.Errorf("interrupted with signal %s, aborting", sig.String()),
					level: logFatal,
				}
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer log.Println("done with server")
		defer wg.Done()
		err := srv.ListenAndServe()
		if err == http.ErrServerClosed {
			log.Println(err)
		} else {
			errors <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer log.Println("done with others")
		defer wg.Done()
		err = eg.Wait()
		if err != nil {
			errors <- err
		}
	}()

	log.Println(<-errors)

	err = srv.Shutdown(ctx)
	if err != nil {
		log.Println(err)
	}

	cleanup := func() {
		log.Println("clean up")
		cancel()
		intervalTicker.Stop()
		close(incomingItems)
		close(changeCh)
		close(errCh)
	}

	cleanup()
	log.Println("clean up done")

	wg.Wait()

	log.Println("END")
}
