package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var streams = map[string]string{
	"btcusdt": "wss://fstream.binance.com/stream?streams=btcusdt@markPrice/btcusdt@depth",
	"ethusdt": "wss://fstream.binance.com/stream?streams=ethusdt@markPrice/ethusdt@depth",
	"solusdt": "wss://fstream.binance.com/stream?streams=solusdt@markPrice/solusdt@depth",
}

var (
	currMarkPrice  = 0.0
	prevMarkPrice  = 0.0
	fundingRate    = "n/a"
	obMutex        = &sync.RWMutex{}
	orderBook      = NewOrderbook()
	ARROW_UP       = "↑"
	ARROW_DOWN     = "↓"
	selectedSymbol = "btcusdt"
	conn           *websocket.Conn
	connMutex      = &sync.Mutex{}
)

const (
	retryLimit = 8
	retryDelay = 10 * time.Second
)

type OrderbookEntry struct {
	Price  float64
	Volume float64
}

// Orderbook structure and methods
type Orderbook struct {
	Asks map[float64]float64
	Bids map[float64]float64
}

func NewOrderbook() *Orderbook {
	return &Orderbook{
		Asks: make(map[float64]float64),
		Bids: make(map[float64]float64),
	}
}

// sorting functions
type byBestAsk []OrderbookEntry

func (a byBestAsk) Len() int {
	return len(a)
}

func (a byBestAsk) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a byBestAsk) Less(i, j int) bool {
	return a[i].Price < a[j].Price
}

type byBestBid []OrderbookEntry

func (a byBestBid) Len() int {
	return len(a)
}

func (a byBestBid) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a byBestBid) Less(i, j int) bool {
	return a[i].Price > a[j].Price
}

func (ob *Orderbook) addAsk(price, volume float64) {
	if volume == 0 {
		delete(ob.Asks, price)
		return
	}
	ob.Asks[price] = volume
}

func (ob *Orderbook) addBid(price, volume float64) {
	if volume == 0 {
		delete(ob.Bids, price)
		return
	}
	ob.Bids[price] = volume
}

func (ob *Orderbook) handleDepthResponse(asks, bids []any) {
	obMutex.Lock()
	defer obMutex.Unlock()

	for _, v := range asks {
		ask := v.([]any)
		price, _ := strconv.ParseFloat(ask[0].(string), 64)
		volume, _ := strconv.ParseFloat(ask[1].(string), 64)
		ob.addAsk(price, volume)
	}

	for _, v := range bids {
		ask := v.([]any)
		price, _ := strconv.ParseFloat(ask[0].(string), 64)
		volume, _ := strconv.ParseFloat(ask[1].(string), 64)
		ob.addBid(price, volume)
	}
}

func (ob *Orderbook) getSortedAsks() []OrderbookEntry {
	obMutex.RLock()
	defer obMutex.RUnlock()

	depth := 10
	entries := make([]OrderbookEntry, 0, len(ob.Asks))

	for price, volume := range ob.Asks {
		entries = append(entries, OrderbookEntry{Price: price, Volume: volume})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Price < entries[j].Price })

	if len(entries) > depth {
		return entries[:depth]
	}

	return entries
}

func (ob *Orderbook) getSortedBids() []OrderbookEntry {
	obMutex.RLock()
	defer obMutex.RUnlock()

	depth := 10
	entries := make([]OrderbookEntry, 0, len(ob.Bids))

	for price, volume := range ob.Bids {
		entries = append(entries, OrderbookEntry{Price: price, Volume: volume})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Price > entries[j].Price })

	if len(entries) >= depth {
		return entries[:depth]
	}

	return entries
}

func getWebSocketEndPoint(symbol string) string {
	return streams[symbol]
}

func startWebSocketProcessing(symbol string) {
	endpoint := getWebSocketEndPoint(symbol)

	connMutex.Lock()
	defer connMutex.Unlock()

	for attempt := 1; attempt <= retryLimit; attempt++ {
		if conn != nil {
			fmt.Println("Closing previous WebSocket connection...")
			conn.Close()
		}

		newConn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
		if err != nil {
			fmt.Printf("Websocket connection failed (attempt %d of %d)", attempt, retryLimit, err)
			time.Sleep(retryDelay)
			continue
		}

		conn = newConn
		fmt.Println("Connected to Websocket")

		go processWebSocketStream(symbol)
		return
	}
	log.Fatalf("Unable to connect to Webhook after %d attempts", retryLimit)
}

func processWebSocketStream(symbol string) {
	var result map[string]interface{}
	for {
		err := conn.ReadJSON(&result)
		if err != nil {
			log.Println("Error reading WebSocket message:", err)
			return
		}

		stream := result["stream"]
		fmt.Println("Processing Stream data:", stream)

		if stream == symbol+"@depth" {
			data := result["data"].(map[string]interface{})
			asks := data["a"].([]interface{})
			bids := data["b"].([]interface{})
			orderBook.handleDepthResponse(asks, bids)
			fmt.Println("Processed Depth Data!")
		}

		if stream == symbol+"@markPrice" {
			data := result["data"].(map[string]interface{})
			priceStr := data["p"].(string)
			fundingRateStr := data["r"].(string)
			prevMarkPrice = currMarkPrice
			currMarkPrice, _ = strconv.ParseFloat(priceStr, 64)
			fundingRate = fundingRateStr
			fmt.Println("Updated Mark Price to:", currMarkPrice, "with funding rate:", fundingRate)
		}
	}
}

func switchSymbol(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")

	if symbol == "" || streams[symbol] == "" {
		http.Error(w, "Invalid symbol", http.StatusBadRequest)
		return
	}

	connMutex.Lock()
	if conn != nil {
		conn.Close()
	}
	connMutex.Unlock()

	selectedSymbol = symbol
	orderBook = NewOrderbook()
	go startWebSocketProcessing(symbol)
	fmt.Fprintln(w, "Symbol switched to:", symbol)
}

func getPriceDirection() string {
	if currMarkPrice > prevMarkPrice {
		return ARROW_UP
	}

	return ARROW_DOWN
}

func getMarketData(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"markPrice":   currMarkPrice,
		"priceArrow":  getPriceDirection(),
		"fundingRate": fundingRate,
		"asks":        orderBook.getSortedAsks(),
		"bids":        orderBook.getSortedBids(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	fmt.Println("Send Market Data Response")
}

func main() {
	fs := http.FileServer(http.Dir("./public"))
	http.Handle("/", fs)

	http.HandleFunc("/api/marketdata", getMarketData)
	http.HandleFunc("/api/switchsymbol", switchSymbol)

	go startWebSocketProcessing(selectedSymbol)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Default port if not specified
		fmt.Println("Defaulting to port", port)
	}
	fmt.Println("Starting HTTP Server on Port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
