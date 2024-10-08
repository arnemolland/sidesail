package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LayerTwo-Labs/sidesail/faucet/drivechaind"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/gorilla/mux"
	"github.com/jessevdk/go-flags"
	"github.com/samber/lo"
)

func main() {
	var opts Options
	_, err := flags.Parse(&opts)
	if err != nil {
		log.Fatalf("could not parse flags: %s", err)
	}

	sender, err := drivechaind.NewClient(opts.RPCHost, opts.RPCUser, opts.RPCPassword)
	if err != nil {
		fmt.Println(fmt.Errorf("could not create client: %w", err))
		return
	}
	defer sender.Disconnect()

	faucet := NewFaucet(sender)

	r := mux.NewRouter()
	r.HandleFunc("/claim", faucet.dispenseCoins).Methods("POST")
	r.HandleFunc("/listclaims", faucet.listClaims).Methods("GET")

	http.Handle("/", corsMiddleware(r))

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("unable to serve HTTP: %s", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedOrigin := "https://drivechain.live"
		origin := r.Header.Get("Origin")

		if origin == allowedOrigin || strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

type Options struct {
	RPCUser     string `short:"u" long:"user" description:"RPC user" required:"true" env:"RPC_USER" default:"user"`
	RPCPassword string `short:"p" long:"password" description:"RPC passwore" required:"true" env:"RPC_PASSWORD" default:"password"`
	RPCHost     string `short:"h" long:"host" description:"RPC url:port" required:"true" env:"RPC_URL" default:"localhost:8332"`
}

type Faucet struct {
	sender         *drivechaind.Client
	mu             sync.Mutex
	dispensed      map[string]bool
	dispensedIP    map[string]bool
	totalDispensed int
}

const (
	CoinsPerRequest = 1
	MaxCoinsPer5Min = 100
)

func NewFaucet(sender *drivechaind.Client) *Faucet {

	faucet := &Faucet{
		sender:         sender,
		dispensed:      make(map[string]bool),
		totalDispensed: 0,
	}

	go func() {
		if err := faucet.resetHandler(); err != nil {
			log.Printf("unable to reset faucet handler: %s", err)
		}
	}()

	return faucet
}

func (f *Faucet) resetHandler() error {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	connectionTicker := time.NewTicker(time.Minute)
	defer connectionTicker.Stop()

	for {
		select {
		case <-ticker.C:
			f.mu.Lock()
			f.totalDispensed = 0
			f.dispensed = make(map[string]bool)
			f.dispensedIP = make(map[string]bool)
			f.mu.Unlock()
			log.Println("faucet reset: cleared total dispensed coins and address list.")
		case <-connectionTicker.C:
			height, err := f.sender.Ping()
			if err != nil {
				return fmt.Errorf("could not ping sender: %w", err)
			}
			log.Println("client ping: still connected at height", height)
		}
	}
}

type DispenseRequest struct {
	Destination string `json:"destination"`
	Amount      string `json:"amount"`
}

func (f *Faucet) validateDispenseArgs(req DispenseRequest) (btcutil.Amount, drivechaind.TransferType, error, int) {
	if req.Destination == "" {
		return 0, "", fmt.Errorf("'destination' must be set"), http.StatusBadRequest
	}

	amountFloat, err := strconv.ParseFloat(req.Amount, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%s is not a valid number", req.Amount), http.StatusBadRequest
	}

	if amountFloat > 1 || amountFloat == 0 {
		return 0, "", fmt.Errorf("amount must be less than 1, and greater than zero"), http.StatusBadRequest
	}

	amount, err := btcutil.NewAmount(amountFloat)
	if err != nil {
		return 0, "", fmt.Errorf("%.8f is not a valid bitcoin amount, expected format like 0.12345678", amountFloat), http.StatusBadRequest
	}

	if f.totalDispensed >= MaxCoinsPer5Min {
		return 0, "", fmt.Errorf("faucet limit reached, try again later"), http.StatusTooManyRequests
	}

	if f.dispensed[req.Destination] {
		return 0, "", fmt.Errorf("address have already received coins"), http.StatusForbidden
	}

	var transferType drivechaind.TransferType
	// first check whether a mainchain address
	_, mainchainErr := btcutil.DecodeAddress(req.Destination, &chaincfg.MainNetParams)
	// then check whether its a sidechain deposit address
	sidechainErr := drivechaind.CheckValidDepositAddress(req.Destination)
	switch {
	case mainchainErr == nil:
		transferType = drivechaind.Mainchain

	case sidechainErr == nil:
		transferType = drivechaind.Sidechain

	default:
		return 0, "", fmt.Errorf("%s is not a valid bitcoin address nor sidechain address", req.Destination), http.StatusBadRequest
	}

	return amount, transferType, nil, 0
}

func getIPFromRequest(r *http.Request) string {
	ip := r.RemoteAddr
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ip = forwarded
	} else if realIP := r.Header.Get("X-Real-Ip"); realIP != "" {
		ip = realIP
	}
	return ip
}

func (f *Faucet) dispenseCoins(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var req DispenseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request: must set address and amount", http.StatusBadRequest)
		return
	}

	amount, transferType, err, status := f.validateDispenseArgs(req)
	if err != nil {
		writeError(w, err.Error(), status)
		return
	}

	ip := getIPFromRequest(r)
	if f.dispensedIP[ip] {
		writeError(w, "dispense threshold exceeded", http.StatusTooManyRequests)
		return
	}

	f.dispensed[req.Destination] = true
	f.dispensed[ip] = true
	f.totalDispensed += CoinsPerRequest

	txid, err := f.sender.SendCoins(req.Destination, amount, transferType)
	if err != nil {
		// undo the dispensation
		f.dispensed[req.Destination] = false
		f.dispensed[ip] = false
		f.totalDispensed -= CoinsPerRequest

		err := fmt.Sprintf("could not dispense coins: %s", err)

		writeError(w, err, http.StatusBadRequest)
		return
	}

	fmt.Printf("sent %.8f to %s in %s\n", amount.ToBTC(), req.Destination, txid.String())

	response := map[string]string{
		"txid": txid.String(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (f *Faucet) listClaims(w http.ResponseWriter, r *http.Request) {

	txs, err := f.sender.ListTransactions()
	if err != nil {
		err := fmt.Sprintf("could not list transactions: %s", err)
		fmt.Println(err)

		http.Error(w, err, http.StatusBadRequest)
		return
	}

	onlyWithdrawals := lo.Filter(txs, func(tx btcjson.ListTransactionsResult, index int) bool {
		// we only want to show withdrawals going from our wallet
		return tx.Amount <= 0
	})

	withPositiveAmounts := lo.Map(onlyWithdrawals, func(tx btcjson.ListTransactionsResult, index int) btcjson.ListTransactionsResult {
		// and the amounts makes most sense when positive
		tx.Amount = math.Abs(tx.Amount)
		if tx.Fee != nil {
			fee := math.Abs(*tx.Fee)
			tx.Fee = &fee
		}
		return tx
	})

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(withPositiveAmounts); err != nil {
		err := fmt.Sprintf("could not encode claims: %s", err)
		fmt.Println(err)

		http.Error(w, err, http.StatusInternalServerError)
	}
}

func writeError(w http.ResponseWriter, error string, status int) {
	http.Error(w, error, status)
}
