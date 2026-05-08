package binance

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type PriceUpdate struct {
	Symbol string
	Price  float64
	Time   int64
}

type Client struct {
	url      string
	conn     *websocket.Conn
	mu       sync.RWMutex
	prices   map[string]float64
	history  map[string][]PriceUpdate
	lastTick time.Time
	subs     []PriceHandler
	stopChan chan struct{}
	symbols  []string
}

type PriceHandler func(PriceUpdate)

func NewClient(url string) *Client {
	return &Client{
		url:      url,
		prices:   make(map[string]float64),
		history:  make(map[string][]PriceUpdate),
		lastTick: time.Time{},
		stopChan: make(chan struct{}),
	}
}

func (c *Client) Connect(symbols []string) error {
	c.symbols = append(c.symbols, symbols...)
	go c.wsLoop(symbols)
	go c.restFallbackLoop()
	return nil
}

func (c *Client) wsLoop(symbols []string) {
	streams := make([]string, len(symbols))
	for i, s := range symbols {
		// Use miniTicker stream for smoother, high-frequency UI updates.
		// Trade stream can appear "stuck" in some environments despite WS being open.
		streams[i] = strings.ToLower(s) + "@miniTicker"
	}
	wsURL := fmt.Sprintf("%s/stream?streams=%s", c.url, strings.Join(streams, "/"))

	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-c.stopChan:
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			log.Printf("[Binance WS] Connect failed: %v, retry in %v", err, backoff)
			time.Sleep(backoff)
			backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
			continue
		}
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		backoff = 1 * time.Second
		log.Printf("[Binance WS] Connected to %s", wsURL)

		c.readLoop(conn)

		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}
}

func (c *Client) readLoop(conn *websocket.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(3 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-c.stopChan:
				return
			case <-ticker.C:
				c.mu.RLock()
				co := c.conn
				c.mu.RUnlock()
				if co != nil {
					co.WriteMessage(websocket.PingMessage, nil)
				}
			}
		}
	}()

	for {
		select {
		case <-c.stopChan:
			return
		case <-done:
			return
		default:
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[Binance WS] Read error: %v", err)
				return
			}
			c.handleMessage(msg)
		}
	}
}

func (c *Client) restFallbackLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			c.mu.RLock()
			hasWS := c.conn != nil
			pricesLen := len(c.prices)
			lastTick := c.lastTick
			c.mu.RUnlock()

			// Fallback criteria:
			// 1) No WS connection
			// 2) No cached prices yet
			// 3) WS connected but no fresh tick for a while (stalled stream)
			staleWS := !lastTick.IsZero() && time.Since(lastTick) > 15*time.Second
			if !hasWS || pricesLen == 0 || staleWS {
				c.fetchRESTPrices()
			}
		}
	}
}

func (c *Client) fetchRESTPrices() {
	for _, sym := range c.symbols {
		url := fmt.Sprintf("https://api.binance.com/api/v3/ticker/price?symbol=%s", strings.ToUpper(sym))
		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var data struct {
			Symbol string `json:"symbol"`
			Price  string `json:"price"`
		}
		if json.Unmarshal(body, &data) != nil {
			continue
		}
		var price float64
		fmt.Sscanf(data.Price, "%f", &price)
		if price > 0 {
			c.mu.Lock()
			c.prices[data.Symbol] = price
			nowMs := time.Now().UnixMilli()
			c.history[data.Symbol] = append(c.history[data.Symbol], PriceUpdate{
				Symbol: data.Symbol,
				Price:  price,
				Time:   nowMs,
			})
			c.lastTick = time.Now()
			c.mu.Unlock()
		}
	}
}

func (c *Client) Subscribe(handler PriceHandler) {
	c.mu.Lock()
	c.subs = append(c.subs, handler)
	c.mu.Unlock()
}

func (c *Client) GetPrice(symbol string) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.prices[symbol]
	return p, ok
}

func (c *Client) GetTWAP(symbol string, windowSec int64) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if windowSec <= 0 {
		windowSec = 45
	}
	h := c.history[symbol]
	if len(h) == 0 {
		return 0, false
	}
	cutoff := time.Now().Add(-time.Duration(windowSec) * time.Second).UnixMilli()
	sum := 0.0
	n := 0
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Time < cutoff {
			break
		}
		if h[i].Price > 0 {
			sum += h[i].Price
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

func (c *Client) Close() {
	close(c.stopChan)
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

func (c *Client) handleMessage(msg []byte) {
	var resp struct {
		Stream string `json:"stream"`
		Data   struct {
			EventType string `json:"e"`
			Symbol    string `json:"s"`
			Price     string `json:"p"` // trade price (for @trade)
			Close     string `json:"c"` // close price (for @miniTicker)
			Time      int64  `json:"T"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg, &resp); err != nil {
		return
	}

	priceStr := resp.Data.Price
	if priceStr == "" {
		priceStr = resp.Data.Close
	}
	if priceStr == "" {
		return
	}

	var price float64
	fmt.Sscanf(priceStr, "%f", &price)
	if price <= 0 {
		return
	}

	c.mu.Lock()
	c.prices[resp.Data.Symbol] = price
	nowMs := time.Now().UnixMilli()
	updateTime := resp.Data.Time
	if updateTime <= 0 {
		updateTime = nowMs
	}
	c.history[resp.Data.Symbol] = append(c.history[resp.Data.Symbol], PriceUpdate{
		Symbol: resp.Data.Symbol,
		Price:  price,
		Time:   updateTime,
	})
	// keep only recent 5 minutes of samples
	cutoff := nowMs - 5*60*1000
	h := c.history[resp.Data.Symbol]
	idx := 0
	for idx < len(h) && h[idx].Time < cutoff {
		idx++
	}
	if idx > 0 {
		c.history[resp.Data.Symbol] = append([]PriceUpdate(nil), h[idx:]...)
	}
	c.lastTick = time.Now()
	handlers := make([]PriceHandler, len(c.subs))
	copy(handlers, c.subs)
	c.mu.Unlock()

	update := PriceUpdate{
		Symbol: resp.Data.Symbol,
		Price:  price,
		Time:   resp.Data.Time,
	}

	for _, h := range handlers {
		h(update)
	}
}
