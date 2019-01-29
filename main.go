package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	hnPollTime           = 5 * time.Minute
	defaultPort          = 8080

	rateLimit          = "5-M"
	eightHrs           = 8 * 60 * 60
	humanTrackingLimit = 300
	itemFromFile       = "file"
	itemFromHN         = "hn"
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

type unixTime int64

type change struct {
	Action changeAction
	Item   *item
}

func (c change) String() string {
	return fmt.Sprintf("%s : %d", c.Action, c.Item.ID)
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

	appStorage := os.Getenv("APP_STORAGE")
	if appStorage == "" {
		appStorage = "./app.json"
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

	var x8h *app

	storageFile, err := os.Open(appStorage)
	if err != nil {
		log.Println(err)
	}
	err = json.NewDecoder(storageFile).Decode(&x8h)
	if err != nil {
		log.Println(err)
	}

	if x8h == nil {
		queue := &limitQueue{
			Limit: humanTrackingLimit,
			Keys:  []int{},
			Store: make(map[int]*item),
		}

		x8h = &app{
			LimitQueue: queue,
			VisitCount: 0,
		}
	}

	defer func() {
		// dump stories at the end
		appDump, err := json.Marshal(x8h)
		if err != nil {
			log.Println(err)
			return
		}
		log.Println(ioutil.WriteFile(appStorage, appDump, 0644))
	}()

	incomingItems := make(chan *item)

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
		var items []int
		err = decoder.Decode(&items)
		if err != nil {
			return nil, err
		}
		if len(items) < limit {
			limit = len(items)
		}

		return items[:limit], nil
	}

	fetchStoriesFromFile := func(inputFilePath string) ([]*item, error) {
		f, err := os.Open(inputFilePath)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		dec := json.NewDecoder(f)
		var dump *app
		err = dec.Decode(&dump)
		if err != nil {
			return nil, err
		}
		var items []*item
		for _, it := range dump.LimitQueue.Store {
			items = append(items, it)
		}
		return items, nil
	}

	serverInputsToUserFetcher := func(ctx context.Context, inputFilePath string) error {
		items, err := fetchStoriesFromFile(inputFilePath)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.From == "" {
				it.From = itemFromFile
			}

			incomingItems <- it
		}

		return nil
	}

	fetchItem := func(ctx context.Context, itemID int) (*item, error) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(storyLink, itemID), nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()

		decoder := json.NewDecoder(resp.Body)
		var it item
		err = decoder.Decode(&it)
		if err != nil {
			return nil, err
		}

		return &it, nil
	}

	topStoriesFetcher := func(ctx context.Context, limit int) error {
		// send items
		itemIds, err := fetchTopStories(ctx, limit)
		if err != nil {
			return err
		}
		for _, itID := range itemIds {
			if appCtx.Err() != nil {
				return nil
			}

			item, err := fetchItem(ctx, itID)
			if err != nil {
				log.Println(err) // warning
			} else {
				incomingItems <- item
			}
		}

		return nil
	}

	storyLister := func(ctx context.Context) {
		for item := range incomingItems {
			func() {
				x8h.Lock()
				defer x8h.Unlock()

				if item.Added == 0 {
					item.Added = unixTime(time.Now().Unix())
				}
				if item.Domain == "" {
					domain, err := urlToDomain(item.URL)
					if err == nil {
						item.Domain = domain
					} else {
						log.Println(err)
					}
				}
				if item.From != itemFromFile { // from file
					item.DiscussLink = fmt.Sprintf(hnPostLink, item.ID)
				}

				removedItemIfAny := x8h.LimitQueue.add(item)

				if removedItemIfAny != nil {
					changeCh <- change{
						Action: changeRemove,
						Item:   removedItemIfAny,
					}
				}

				changeCh <- change{
					Action: changeAdd,
					Item:   item,
				}

			}()
		}
	}

	storyRemover := func(ctx context.Context) error {
		topItems, err := fetchTopStories(ctx, frontPageNumArticles)
		if err != nil {
			return err
		}

		fileItems, err := fetchStoriesFromFile(appStorage)
		if err != nil {
			return err
		}

		x8h.Lock()
		defer x8h.Unlock()
		for id, it := range x8h.LimitQueue.Store {
			if ctx.Err() != nil {
				return nil
			}

			stillAtTop := false
			switch it.From {
			case itemFromFile:
				for _, it := range fileItems {
					if id == it.ID {
						stillAtTop = true
						break
					}
				}

			default:
				for _, tid := range topItems {
					if tid == id {
						stillAtTop = true
						break
					}
				}
			}

			if !stillAtTop && time.Since(time.Unix(int64(it.Added), 0)).Seconds() > eightHrs {
				// send these to removed chan
				changeCh <- change{
					Action: changeRemove,
					Item:   x8h.LimitQueue.Store[id],
				}
				x8h.LimitQueue.remove(id)
			}
		}
		return nil
	}

	changeLogger := func() {
		for c := range changeCh {
			log.Println(c)
		}
	}

	errLogger := func() {
		for err := range errCh {
			log.Println(err)
		}
	}

	const headerXForwardedFor = "X-Forwarded-For"
	const headerXRealIP = "X-Real-IP"
	realIP := func(r *http.Request) string {
		ra := r.RemoteAddr
		if ip := r.Header.Get(headerXForwardedFor); ip != "" {
			ra = strings.Split(ip, ", ")[0]
		} else if ip := r.Header.Get(headerXRealIP); ip != "" {
			ra = ip
		} else {
			ra, _, _ = net.SplitHostPort(ra)
		}

		return ra
	}

	http.Handle("/", middleware.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(realIP(r))
		log.Println(r.UserAgent())

		x8h.Lock()
		defer x8h.Unlock()

		data := make(map[string]interface{})
		data["Data"] = x8h.LimitQueue.Store

		x8h.VisitCount++

		data["Visits"] = x8h.VisitCount
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
				log.Println("starting file stories fetcher")
				return serverInputsToUserFetcher(ctx, appStorage)
			})
			eg.Go(func() error {
				log.Println("starting story remover")
				return storyRemover(ctx)
			})
			err := eg.Wait()
			if err != nil {
				errCh <- appErr{
					err:   err,
					level: logWarning,
				}
			}
		}
	}()

	log.Println("starting error logger")
	go errLogger()

	log.Println("starting change logger")
	go changeLogger()

	log.Println("starting top stories fetcher")
	go topStoriesFetcher(appCtx, frontPageNumArticles)

	go func() {
		err := serverInputsToUserFetcher(appCtx, appStorage)
		if err != nil {
			log.Println(err)
		}
	}()

	log.Println("starting story lister")
	go storyLister(appCtx)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port)}

	serverErrors := make(chan error)
	go func() {
		serverErrors <- srv.ListenAndServe()
	}()

	select {
	case sig := <-stop:
		log.Printf("interrupted with signal %s, aborting\n", sig.String())
	case err := <-serverErrors:
		log.Println(err)
	}

	ctx, c := context.WithTimeout(appCtx, 2*time.Second)
	defer c()
	err = srv.Shutdown(ctx)
	if err != nil {
		log.Println(err)
	}

	cleanup := func() {
		cancel()
		intervalTicker.Stop()
		close(incomingItems)
		close(changeCh)
		close(errCh)
	}

	log.Println("clean up")
	cleanup()
	log.Println("clean up done")

	log.Println("END")
}
