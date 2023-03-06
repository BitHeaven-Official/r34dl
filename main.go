package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/logrusorgru/aurora"
	"golang.org/x/net/proxy"
)

type Post struct {
	PreviewURL   string `json:"preview_url"`
	SampleURL    string `json:"sample_url"`
	FileURL      string `json:"file_url"`
	Directory    int    `json:"directory"`
	Hash         string `json:"hash"`
	Height       int    `json:"height"`
	ID           int    `json:"id"`
	Image        string `json:"image"`
	Change       int    `json:"change"`
	Owner        string `json:"owner"`
	ParentID     int    `json:"parent_id"`
	Rating       string `json:"rating"`
	Sample       int    `json:"sample"`
	SampleHeight int    `json:"sample_height"`
	SampleWidth  int    `json:"sample_width"`
	Score        int    `json:"score"`
	Tags         string `json:"tags"`
	Width        int    `json:"width"`
}

// SetProxy
// set http proxy in the format `http://HOST:PORT`
// set socket proxy in the format `socks5://HOST:PORT`
func SetProxy(proxyAddr string, timeout int) (http.Transport, error) {
	if strings.HasPrefix(proxyAddr, "http") {
		urlproxy, err := url.Parse(proxyAddr)
		if err != nil {
			return http.Transport{}, err
		}
		transport := &http.Transport{
			Proxy:        http.ProxyURL(urlproxy),
			TLSNextProto: make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
			DialContext: (&net.Dialer{
				Timeout: time.Duration(timeout) * time.Second,
			}).DialContext,
		}
		return *transport, nil
	}
	if strings.HasPrefix(proxyAddr, "socks5") {
		baseDialer := &net.Dialer{
			Timeout:   time.Duration(timeout) * time.Second,
			KeepAlive: time.Duration(timeout) * time.Second,
		}
		socksHostPort := strings.ReplaceAll(proxyAddr, "socks5://", "")
		dialSocksProxy, err := proxy.SOCKS5("tcp", socksHostPort, nil, baseDialer)
		if err != nil {
			return http.Transport{}, errors.New("error creating socks5 proxy :" + err.Error())
		}
		if contextDialer, ok := dialSocksProxy.(proxy.ContextDialer); ok {
			dialContext := contextDialer.DialContext
			transport := &http.Transport{
				DialContext: dialContext,
			}
			return *transport, nil
		} else {
			return http.Transport{}, errors.New("failed type assertion to DialContext")
		}

	}
	return http.Transport{}, errors.New("only support http(s) or socks5 protocol")
}

func GetPosts(tags string, page int, limit int, transport http.Transport) ([]Post, error) {
	client := &http.Client{Transport: &transport}
	posts := []Post{}

	req, _ := http.NewRequest("GET", "https://api.rule34.xxx/index.php", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.3; Win64; x64; rv:6.1) Gecko/20100101 Firefox/6.1.9")

	qs := req.URL.Query()
	qs.Add("page", "dapi")
	qs.Add("s", "post")
	qs.Add("q", "index")
	qs.Add("json", "1")
	qs.Add("tags", tags)
	qs.Add("limit", strconv.Itoa(limit))
	qs.Add("pid", strconv.Itoa(page))

	req.URL.RawQuery = qs.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(body, &posts)
	if err != nil {
		return nil, err
	}

	return posts, nil
}

// workState stores the state of all the jobs and
// is shared across workers
type workState struct {
	Total         int
	Completed     int
	Successes     int
	Failures      int
	SaveDirectory string
}

// BeginDownload takes a slice of posts, a directory to save them in, and a
// number of concurrent workers to make. It blocks until all the post have
// been processed. It returns the number of successes, failures, and the total
// amount of posts.
func BeginDownload(posts *[]Post, saveDirectory *string, maxConcurrents *int, transport http.Transport) (*int, *int, *int) {
	// Channel for main goroutine to give workers a post when they are done downloading one
	wc := make(chan *Post)

	var current int

	total := len(*posts)

	state := workState{
		Total:         total,
		SaveDirectory: *saveDirectory,
	}

	// If we have more workers than posts, then we don't need all of them
	if *maxConcurrents > total {
		*maxConcurrents = total
	}

	for i := 0; i < *maxConcurrents; i++ {
		// Create our workers
		go work(i+1, &state, wc, transport)

		// Give them their initial posts
		wc <- &(*posts)[current]
		current++

		time.Sleep(time.Millisecond * 50)
	}

	for {
		// Wait for a worker to be done (they send nil to wc)
		<-wc

		// If we finished downloading all posts, break out of the loop
		if state.Successes+state.Failures == total {
			break
		}

		// If there's no more posts to give, stop the worker
		if current >= total {
			wc <- nil
			continue
		}

		// Give the worker the next post in the array
		wc <- &(*posts)[current]
		current++
	}

	return &state.Successes, &state.Failures, &total
}

func work(wn int, state *workState, wc chan *Post, transport http.Transport) {
	for {
		state.Completed++

		// Wait for a post from main
		post := <-wc
		if post == nil { // nil means there aren't any more posts, so we're OK to break
			return
		}

		progress := aurora.Sprintf(aurora.Green("[%d/%d]"), state.Completed, state.Total)
		workerText := aurora.Sprintf(aurora.Cyan("[w%d]"), wn)

		fmt.Println(aurora.Sprintf(
			"%s %s Downloading post %d -> %s",
			progress,
			workerText,
			post.ID,
			getSavePath(post, &state.SaveDirectory),
		))

		err := downloadPost(post, state.SaveDirectory, transport)
		if err != nil {
			fmt.Printf("[w%d] Failed to download post %d: %v\n", wn, post.ID, err)
			state.Failures++
		} else {
			state.Successes++
		}

		// Signal to main goroutine that we are done with this download
		wc <- nil
	}
}

func getSavePath(post *Post, directory *string) string {
	savePath := path.Join(*directory, strconv.Itoa(post.ID)+path.Ext(post.FileURL))
	return savePath
}

func downloadPost(post *Post, directory string, transport http.Transport) error {
	savePath := getSavePath(post, &directory)

	if _, err := os.Stat(savePath); err == nil {
		fmt.Print("File exists, skip...\n")
	} else {
		resp, err := HTTPGet(post.FileURL, transport)
		if err != nil {
			return err
		}

		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		err = ioutil.WriteFile(savePath, body, 0755)
		if err != nil {
			return err
		}
	}

	return nil
}

// HTTPGet is a helper function that automatically adds the
// tool's UA to an HTTP GET request
func HTTPGet(url string, transport http.Transport) (*http.Response, error) {
	client := &http.Client{Transport: &transport}

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 6.3; Win64; x64; rv:6.1) Gecko/20100101 Firefox/6.1.9")

	return client.Do(req)
}

func main() {
	tags := flag.String("tags", "", "Tags to search for")
	maxConcurrents := flag.Int("concurrents", 30, "Maximum amount of concurrent downloads")
	saveDirectory := flag.String("out", "dl", "The directory to write the downloaded posts to")
	postLimit := flag.Int("limit", 9999999999, "Maximum amount of posts to grab from e621")
	proxyAddr := flag.String("proxy", "", "Proxy address to parsing")
	timeout := flag.Int("timeout", 10, "Timeout proxy to parsing")

	flag.Parse()

	transport, err := SetProxy(*proxyAddr, *timeout)
	if err != nil {
		fmt.Println("[WARN] Proxy is not using or proxy error!")
	}

	allPosts := []Post{}
	nlimit := (*postLimit)

	i := 1
	for {
		fmt.Printf("Fetching page %d...", i)

		currentPostsCount := 100

		nlimit = nlimit - currentPostsCount
		if nlimit <= 0 {
			currentPostsCount += nlimit
		}

		posts, err := GetPosts(*tags, i, currentPostsCount, transport)
		if err != nil {
			panic(err)
		}

		fmt.Printf(" fetched %d posts\n", len(posts))

		if len(posts) == 0 {
			break
		}

		allPosts = append(allPosts, posts...)
		if nlimit <= 0 {
			break
		}

		i++
	}

	fmt.Printf("Found %d posts. Starting download with %d workers...\n\n", len(allPosts), *maxConcurrents)

	cwd, _ := os.Getwd()
	absSaveDir := path.Join(cwd, *saveDirectory)

	err = os.MkdirAll(absSaveDir, 0755)
	if err != nil {
		fmt.Printf("Cannot create output directory (%s). Do you have the right permissions?\n", absSaveDir)
		os.Exit(1)
	}

	successes, failures, _ := BeginDownload(&allPosts, saveDirectory, maxConcurrents, transport)

	fmt.Printf("\nAll done! %d posts downloaded and saved. (%d failed to download)\n", *successes, *failures)
}
