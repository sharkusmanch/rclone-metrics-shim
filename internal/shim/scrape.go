package shim

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	firstScrapeRetry = 100 * time.Millisecond
	scrapeTimeout    = 2 * time.Second
	maxScrapeBytes   = 4 << 20
)

// Scraper polls rclone's metrics endpoint on a unix socket and remembers the
// most recent successful response.
type Scraper struct {
	client   *http.Client
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	mu   sync.Mutex
	body []byte
	at   time.Time
}

// StartScraper begins polling in the background. Until the first success it
// retries quickly, because the socket appears shortly after rclone starts.
func StartScraper(socket string, interval time.Duration) *Scraper {
	s := &Scraper{
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		client: &http.Client{
			Timeout: scrapeTimeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
	go s.loop()
	return s
}

func (s *Scraper) loop() {
	defer close(s.done)
	for {
		wait := s.interval
		if !s.scrapeOnce() && !s.have() {
			wait = firstScrapeRetry
		}
		select {
		case <-s.stop:
			return
		case <-time.After(wait):
		}
	}
}

func (s *Scraper) have() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body != nil
}

func (s *Scraper) scrapeOnce() bool {
	resp, err := s.client.Get("http://rclone/metrics")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScrapeBytes))
	if err != nil {
		return false
	}
	s.mu.Lock()
	s.body, s.at = body, time.Now()
	s.mu.Unlock()
	return true
}

// Stop ends polling, waits for the loop to exit and returns the last
// successful scrape. It is safe to call more than once.
func (s *Scraper) Stop() (body []byte, at time.Time, ok bool) {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body, s.at, s.body != nil
}
