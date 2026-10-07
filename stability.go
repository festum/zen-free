package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	dialTimeout             = 15 * time.Second
	tlsHandshakeTimeout     = 10 * time.Second
	responseHeaderTimeout   = 60 * time.Second
	idleConnTimeout         = 90 * time.Second
	expectContinueTimeout   = 1 * time.Second
	maxIdleConns            = 32
	maxIdleConnsPerHost     = 16
	upstreamIdleTimeout     = 90 * time.Second
	clientKeepAliveInterval = 60 * time.Second
	maxAttempts             = 3
	retryBackoffs           = []time.Duration{1 * time.Second, 2 * time.Second}
	retryAfterCap           = 5 * time.Second
	retryLoopBudget         = 30 * time.Second
)

const interruptedStreamPair = "data: {\"error\":{\"message\":\"oc-zen: upstream stream interrupted\",\"type\":\"upstream_interrupted\"}}\n\ndata: [DONE]\n\n"

func newUpstreamClient() *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleConnTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		ForceAttemptHTTP2:     false,
	}
	return &http.Client{Transport: transport}
}

type idleWatchBody struct {
	body          io.ReadCloser
	timeout       time.Duration
	timer         *time.Timer
	done          chan struct{}
	once          sync.Once
	bodyCloseOnce sync.Once
	expired       chan struct{}
}

func watchUpstreamBody(body io.ReadCloser, timeout time.Duration, session string) *idleWatchBody {
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	w := &idleWatchBody{body: body, timeout: timeout, done: make(chan struct{}), expired: make(chan struct{})}
	w.timer = time.AfterFunc(timeout, func() {
		select {
		case <-w.done:
			return
		default:
		}
		close(w.expired)
		log.Printf("idle_watchdog after=%s ses=%s", timeout, session)
		w.closeBody()
	})
	return w
}

func (w *idleWatchBody) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if n > 0 && w.timer != nil {
		w.timer.Reset(w.timeout)
	}
	select {
	case <-w.expired:
		if err == nil {
			err = context.DeadlineExceeded
		}
	default:
	}
	return n, err
}

func (w *idleWatchBody) closeBody() { w.bodyCloseOnce.Do(func() { _ = w.body.Close() }) }

func (w *idleWatchBody) Close() error {
	w.once.Do(func() {
		close(w.done)
		if w.timer != nil {
			w.timer.Stop()
		}
	})
	w.closeBody()
	return nil
}

type serializedStreamWriter struct {
	w        http.ResponseWriter
	mu       sync.Mutex
	reset    chan struct{}
	stop     chan struct{}
	done     chan struct{}
	interval time.Duration
}

func newSerializedStreamWriter(w http.ResponseWriter, interval time.Duration) *serializedStreamWriter {
	s := &serializedStreamWriter{w: w, reset: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), interval: interval}
	go s.heartbeat()
	return s
}

func (s *serializedStreamWriter) Header() http.Header { return s.w.Header() }

func (s *serializedStreamWriter) WriteHeader(status int) {
	s.mu.Lock()
	s.w.WriteHeader(status)
	s.mu.Unlock()
}
func (s *serializedStreamWriter) heartbeat() {
	defer close(s.done)
	if s.interval <= 0 {
		<-s.stop
		return
	}
	timer := time.NewTimer(s.interval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			s.mu.Lock()
			_, _ = io.WriteString(s.w, ": keep-alive\n\n")
			if f, ok := s.w.(http.Flusher); ok {
				f.Flush()
			}
			s.mu.Unlock()
			timer.Reset(s.interval)
		case <-s.reset:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.interval)
		case <-s.stop:
			return
		}
	}
}

func (s *serializedStreamWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.w.Write(p)
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	s.mu.Unlock()
	select {
	case s.reset <- struct{}{}:
	default:
	}
	return n, err
}

func (s *serializedStreamWriter) Flush() {
	s.mu.Lock()
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	s.mu.Unlock()
	select {
	case s.reset <- struct{}{}:
	default:
	}
}

func (s *serializedStreamWriter) Close() {
	close(s.stop)
	<-s.done
}
