package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/middleware/stdlib"
	sim "github.com/ulule/limiter/v3/drivers/store/memory"

	"golang.org/x/sync/errgroup"
)

// json inputs preferred over these
const (
	topStories           = "https://hacker-news.firebaseio.com/v0/topstories.json"
	storyLink            = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnPostLink           = "https://news.ycombinator.com/item?id=%d"
	frontPageNumArticles = 30
	hnPollTime           = 5 * 60 // 5 mintute
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

	devMode  appMode = "dev"
	prodMode         = "prod"
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
	appStorage := os.Getenv("APP_STORAGE")
	if appStorage == "" {
		appStorage = "./app.json"
	}

	var x8h *app

	storageFile, err := os.Open(appStorage)
	if err != nil {
		panic(err)
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
			Config: config{
				Mode:                 devMode,
				Port:                 defaultPort,
				TemplateFilePath:     "./index.html",
				TopStoriesURL:        topStories,
				StoryURL:             storyLink,
				HNPostLink:           hnPostLink,
				FrontPageNumArticles: frontPageNumArticles,
				HNPollTime:           hnPollTime,
				RateLimit:            rateLimit,
				DeletionPeriod:       eightHrs,
				HumanTrackingLimit:   humanTrackingLimit,
				ItemFromFile:         itemFromFile,
				ItemFromHN:           itemFromHN,
			},
		}
	} else {
		for _, it := range x8h.LimitQueue.Store {
			if it.Added == 0 {
				it.Added = unixTime(time.Now().Unix())
			}
		}
	}

	port := x8h.Config.Port
	templateFile := x8h.Config.TemplateFilePath

	tmpl := template.New("index.html")
	tmpl, err = tmpl.ParseFiles(templateFile)
	if err != nil {
		panic(err)
	}

	store := sim.NewStore()
	// Define a limit rate to 5 requests per minute.
	rate, err := limiter.NewRateFromFormatted(x8h.Config.RateLimit)
	if err != nil {
		panic(err)
	}

	middleware := stdlib.NewMiddleware(limiter.New(store, rate, limiter.WithTrustForwardHeader(true)))

	appCtx, cancel := context.WithCancel(context.Background())

	errCh := make(chan appErr)
	changeCh := make(chan change)

	dumpApp := func() error {
		x8h.Lock()
		defer x8h.Unlock()
		// dump stories at the end
		appDump, err := json.MarshalIndent(x8h, "", "  ")
		if err != nil {
			return err
		}
		return ioutil.WriteFile(appStorage, appDump, 0644)
	}

	defer dumpApp()

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
		req, err := http.NewRequest(http.MethodGet, x8h.Config.TopStoriesURL, nil)
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
			if it.From == "" || it.From == itemFromFile {
				items = append(items, it)
			}
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
				it.From = x8h.Config.ItemFromFile
			}

			if it.Added == 0 {
				it.Added = unixTime(time.Now().Unix())
			}

			incomingItems <- it
		}

		return nil
	}

	fetchItem := func(ctx context.Context, itemID int) (*item, error) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(x8h.Config.StoryURL, itemID), nil)
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
				item.From = x8h.Config.ItemFromHN
				incomingItems <- item
			}
		}

		return nil
	}

	existingItemsUpdater := func(ctx context.Context, limit int) error {
		itemIds, err := fetchTopStories(ctx, limit)
		if err != nil {
			return err
		}
		var olderItems []int
		func() {
			x8h.Lock()
			defer x8h.Unlock()
			for ID := range itemIds {
				if _, ok := x8h.LimitQueue.Store[ID]; !ok {
					olderItems = append(olderItems, ID)
				}
			}
		}()

		for ID := range olderItems {
			if appCtx.Err() != nil {
				return nil
			}

			item, err := fetchItem(ctx, ID)
			if err != nil {
				log.Println(err) // warning
			} else {
				item.From = x8h.Config.ItemFromHN
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

				previousItem, ok := x8h.LimitQueue.Store[item.ID]
				if ok {
					item.Added = previousItem.Added
				} else if item.Added == 0 {
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
				if item.From != itemFromFile { // from hn
					item.DiscussLink = fmt.Sprintf(x8h.Config.HNPostLink, item.ID)
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
		topItems, err := fetchTopStories(ctx, x8h.Config.FrontPageNumArticles)
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
			case itemFromHN:
				for _, tid := range topItems {
					if tid == id {
						stillAtTop = true
						break
					}
				}
			case itemFromFile:
				stillAtTop = false // will remove anything from file after 8hrs
			default:
				stillAtTop = false
			}

			if !stillAtTop && time.Since(time.Unix(int64(it.Added), 0)) > x8h.Config.DeletionPeriod*time.Second {
				it := x8h.LimitQueue.remove(id)
				// send these to removed chan
				changeCh <- change{
					Action: changeRemove,
					Item:   it,
				}
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

		data := make(map[string]interface{})
		var buf bytes.Buffer

		func() {
			x8h.Lock()
			defer x8h.Unlock()

			data["Data"] = x8h.LimitQueue.Store
			x8h.VisitCount++
			data["Visits"] = x8h.VisitCount
			data["Version"] = version
			err = tmpl.Execute(&buf, data)
		}()

		if err != nil {
			errCh <- appErr{
				err:   err,
				level: logFatal,
			}

			fmt.Fprintf(w, "error: %v", err)
			return
		}
		_, err = io.Copy(w, &buf)
		if err != nil {
			log.Println(err)
			fmt.Fprintf(w, "error: %v", err)
		}
		return
	})))

	log.Println("START")
	log.Println("starting the app")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt, os.Kill)

	intervalTicker := time.NewTicker(x8h.Config.HNPollTime * time.Second)

	go func() {
		for range intervalTicker.C {
			log.Println("starting ticker ticker")
			eg, ctx := errgroup.WithContext(appCtx)
			eg.Go(func() error {
				return existingItemsUpdater(appCtx, x8h.Config.FrontPageNumArticles)
			})
			eg.Go(func() error {
				log.Println("starting top stories fetcher")
				return topStoriesFetcher(ctx, x8h.Config.FrontPageNumArticles)
			})
			eg.Go(func() error {
				log.Println("starting file stories fetcher")
				return serverInputsToUserFetcher(ctx, appStorage)
			})
			eg.Go(func() error {
				log.Println("starting story remover")
				return storyRemover(ctx)
			})
			eg.Go(func() error {
				log.Println("dumping the app")
				return dumpApp()
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

	log.Println("starting story lister")
	go storyLister(appCtx)

	go func() {
		log.Println("starting top stories fetcher")
		err := serverInputsToUserFetcher(appCtx, appStorage)
		if err != nil {
			log.Println(err)
		}
		err = topStoriesFetcher(appCtx, x8h.Config.FrontPageNumArticles)
		if err != nil {
			log.Println(err)
		}
	}()

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port)}

	serverErrors := make(chan error)
	go func() {
		err := srv.ListenAndServe()
		if appCtx.Err() == nil && err != http.ErrServerClosed {
			serverErrors <- err
		} else {
			log.Println(err)
		}
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

	defer func() {
		cancel()
		intervalTicker.Stop()
		close(incomingItems)
		close(changeCh)
		close(errCh)
		close(serverErrors)
	}()

	log.Println("END")
}
