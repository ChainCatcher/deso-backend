package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/deso-protocol/backend/apis"

	"github.com/deso-protocol/core/lib"
	"github.com/golang/glog"
	"github.com/montanaflynn/stats"
)

// Index ...
func (fes *APIServer) Index(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "Your DeSo node is running!\n")
}

// NOTE: This is a readiness check not a health check
func (fes *APIServer) HealthCheck(ww http.ResponseWriter, rr *http.Request) {
	// Check that the blockchain is fully current OR the blockchain is in a
	// needs blocks state and the header tip is within 10 blocks of the block tip.
	blockchainHeight := fes.blockchain.BlockTip().Height
	chainState := fes.blockchain.ChainState()
	if chainState != lib.SyncStateFullyCurrent &&
		!(chainState == lib.SyncStateNeedBlocksss &&
			fes.blockchain.HeaderTip().Height-blockchainHeight < 10) {
		_AddInternalServerError(ww, fmt.Sprintf("Waiting for blockchain to sync. "+
			"Height: %v, SyncState: %v", blockchainHeight, fes.blockchain.ChainState()))
		return
	}

	// Check that we've received our first transaction bundle. We skip this check
	// if we've disabled networking, since in that case we shouldn't expect to get
	// any mempool messages from our peers.
	if !fes.backendServer.HasProcessedFirstTransactionBundle() &&
		!fes.backendServer.DisableNetworking {
		_AddInternalServerError(ww, "Waiting on mempool to sync")
		return
	}

	// If we have txindex configured then also do a check for that.
	if fes.TXIndex != nil &&
		fes.TXIndex.TXIndexChain.ChainState() != lib.SyncStateFullyCurrent &&
		!(fes.TXIndex.TXIndexChain.ChainState() == lib.SyncStateNeedBlocksss &&
			fes.TXIndex.TXIndexChain.HeaderTip().Height-fes.TXIndex.TXIndexChain.BlockTip().Height < 10) {
		txindexHeight := fes.TXIndex.TXIndexChain.BlockTip().Height

		_AddInternalServerError(ww, fmt.Sprintf("Waiting for txindex to sync. "+
			"Height: %v, SyncState: %v", txindexHeight, fes.TXIndex.TXIndexChain.ChainState()))
		return
	}

	fmt.Fprint(ww, "200")
}

type GetExchangeRateResponse struct {
	// BTC
	SatoshisPerDeSoExchangeRate    uint64
	USDCentsPerBitcoinExchangeRate uint64

	// ETH
	NanosPerETHExchangeRate    uint64
	USDCentsPerETHExchangeRate uint64

	// DESO
	NanosSold                          uint64
	USDCentsPerDeSoExchangeRate        uint64
	USDCentsPerDeSoReserveExchangeRate uint64
	BuyDeSoFeeBasisPoints              uint64
	USDCentsPerDeSoBlockchainDotCom    uint64
	USDCentsPerDeSoCoinbase            uint64 // Deprecated

	SatoshisPerBitCloutExchangeRate        uint64 // Deprecated
	USDCentsPerBitCloutExchangeRate        uint64 // Deprecated
	USDCentsPerBitCloutReserveExchangeRate uint64 // Deprecated
}

func (fes *APIServer) GetExchangeRate(ww http.ResponseWriter, rr *http.Request) {
	readUtxoView, _ := fes.backendServer.GetMempool().GetAugmentedUniversalView()

	// BTC
	usdCentsPerBitcoin := fes.UsdCentsPerBitCoinExchangeRate
	// If we don't have a valid value from monitoring at this time, use the price from the protocol
	if usdCentsPerBitcoin == 0 {
		usdCentsPerBitcoin = float64(readUtxoView.GetCurrentUSDCentsPerBitcoin())
	}

	// ETH
	usdCentsPerETH := fes.UsdCentsPerETHExchangeRate
	nanosPerETH := fes.GetNanosFromETH(big.NewFloat(1), 0)

	usdCentsPerDeSoExchangeRate := fes.GetExchangeDeSoPrice()
	nanosFromSats := fes.GetNanosFromSats(1, 0)
	satoshisPerUnit := uint64(0)
	if nanosFromSats != 0 {
		satoshisPerUnit = lib.NanosPerUnit / fes.GetNanosFromSats(1, 0)
	}

	res := &GetExchangeRateResponse{
		// BTC
		USDCentsPerBitcoinExchangeRate: uint64(usdCentsPerBitcoin),
		SatoshisPerDeSoExchangeRate:    satoshisPerUnit,

		// ETH
		USDCentsPerETHExchangeRate: usdCentsPerETH,
		NanosPerETHExchangeRate:    nanosPerETH,

		// DESO
		NanosSold:                          readUtxoView.NanosPurchased,
		USDCentsPerDeSoExchangeRate:        usdCentsPerDeSoExchangeRate,
		USDCentsPerDeSoReserveExchangeRate: fes.USDCentsToDESOReserveExchangeRate,
		BuyDeSoFeeBasisPoints:              fes.BuyDESOFeeBasisPoints,
		USDCentsPerDeSoCoinbase:            fes.MostRecentCoinbasePriceUSDCents,
		USDCentsPerDeSoBlockchainDotCom:    fes.MostRecentBlockchainDotComPriceUSDCents,

		// Deprecated
		SatoshisPerBitCloutExchangeRate:        satoshisPerUnit,
		USDCentsPerBitCloutExchangeRate:        usdCentsPerDeSoExchangeRate,
		USDCentsPerBitCloutReserveExchangeRate: fes.USDCentsToDESOReserveExchangeRate,
	}

	if err := json.NewEncoder(ww).Encode(res); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GetExchangeRate: Problem encoding response as JSON: %v", err))
		return
	}
}

func (fes *APIServer) GetExchangeDeSoPrice() uint64 {
	// We no longer observe a reserve rate.
	if fes.MostRecentDesoDexPriceUSDCents == 0 {
		return fes.MostRecentGatePriceUSDCents
	}
	return fes.MostRecentDesoDexPriceUSDCents
}

type BlockchainDeSoTickerResponse struct {
	Symbol         string  `json:"symbol"`
	Price24H       float64 `json:"price_24h"`
	Volume24H      float64 `json:"volume_24h"`
	LastTradePrice float64 `json:"last_trade_price"`
}

func (fes *APIServer) GetBlockchainDotComExchangeRate() (_exchangeRate float64, _err error) {
	// Get the ticker from Blockchain.com
	// Do several fetches and take the max
	//
	// TODO: This is due to a bug in Blockchain's API that returns random values ~30% of the
	// time for the last_price field. Once that bug is fixed, this multi-fetching will no
	// longer be needed.
	httpClient := &http.Client{}
	exchangeRatesFetched := []float64{}
	for ii := 0; ii < 10; ii++ {
		url := "https://api.blockchain.com/v3/exchange/tickers/CLOUT-USD"
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			glog.V(2).Infof("GetBlockchainDotComExchangeRate: Problem creating request: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			glog.V(2).Infof("GetBlockchainDotComExchangeRate: Problem with HTTP request %s: %v", url, err)
			continue
		}
		defer resp.Body.Close()

		// Decode the response into the appropriate struct.
		body, _ := ioutil.ReadAll(resp.Body)
		responseData := &BlockchainDeSoTickerResponse{}
		decoder := json.NewDecoder(bytes.NewReader(body))
		if err = decoder.Decode(responseData); err != nil {
			glog.V(2).Infof("GetBlockchainDotComExchangeRate: Problem decoding response JSON into "+
				"interface %v, response: %v, error: %v", responseData, resp, err)
			continue
		}

		// Return the last trade price.
		usdCentsToDeSoExchangePrice := uint64(responseData.LastTradePrice * 100)

		exchangeRatesFetched = append(exchangeRatesFetched, float64(usdCentsToDeSoExchangePrice))
	}
	blockchainDotComExchangeRate, err := stats.Max(exchangeRatesFetched)
	if err != nil {
		glog.V(2).Infof("GetBlockchainDotComExchangeRate: Problem getting max from list of float64s: %v", err)
		return 0, err
	}
	glog.V(2).Infof("Blockchain exchange rate: %v %v", blockchainDotComExchangeRate, exchangeRatesFetched)
	if fes.backendServer != nil && fes.backendServer.GetStatsdClient() != nil {
		if err = fes.backendServer.GetStatsdClient().Gauge("BLOCKCHAIN_LAST_TRADE_PRICE", blockchainDotComExchangeRate, []string{}, 1); err != nil {
			glog.V(2).Infof("GetBlockchainDotComExchangeRate: Error logging Last Trade Price of %f to datadog: %v", blockchainDotComExchangeRate, err)
		}
	}
	return blockchainDotComExchangeRate, nil
}

type CoinbaseDeSoTickerResponse struct {
	Data struct {
		Base     string `json:"base"`
		Currency string `json:"currency"`
		Amount   string `json:"amount"` // In USD
	} `json:"data"`
}

func (fes *APIServer) GetCoinbaseExchangeRate() (_exchangeRate float64, _err error) {
	httpClient := &http.Client{}
	url := "https://api.coinbase.com/v2/prices/DESO-USD/buy"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		glog.Errorf("GetCoinbaseExchangeRate: Problem creating request: %v", err)
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		glog.Errorf("GetCoinbaseExchangeRate: Problem making request: %v", err)
		return 0, err
	}
	defer resp.Body.Close()
	// Decode the response into the appropriate struct.
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		glog.Errorf("GetCoinbaseExchangeRate: Problem reading response body: %v", err)
		return 0, err
	}
	responseData := &CoinbaseDeSoTickerResponse{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err = decoder.Decode(responseData); err != nil {
		glog.Errorf("GetCoinbaseExchangeRate: Problem decoding response JSON into "+
			"interface %v, response: %v, error: %v", responseData, resp, err)
		return 0, err
	}
	usdToDESOExchangePrice, err := strconv.ParseFloat(responseData.Data.Amount, 64)
	if err != nil {
		glog.Errorf("GetCoinbaseExchangeRate: Problem parsing amount as float: %v", err)
		return 0, err
	}

	usdCentsToDESOExchangePrice := usdToDESOExchangePrice * 100
	if fes.backendServer != nil && fes.backendServer.GetStatsdClient() != nil {
		if err = fes.backendServer.GetStatsdClient().Gauge("COINBASE_LAST_TRADE_PRICE", usdCentsToDESOExchangePrice, []string{}, 1); err != nil {
			glog.Errorf("GetCoinbaseExchangeRate: Error logging Last Trade Price of %f to datadog: %v", usdCentsToDESOExchangePrice, err)
		}
	}
	return usdCentsToDESOExchangePrice, nil
}

type GateTickerResponse struct {
	CurrencyPair     string `json:"currency_pair"`
	Last             string `json:"last"`
	LowestAsk        string `json:"lowest_ask"`
	LowestSize       string `json:"lowest_size"`
	HighestBid       string `json:"highest_bid"`
	HighestSize      string `json:"highest_size"`
	ChangePercentage string `json:"change_percentage"`
	BaseVolume       string `json:"base_volume"`
	QuoteVolume      string `json:"quote_volume"`
	High24H          string `json:"high_24h"`
	Low24H           string `json:"low_24h"`
}

type currencyPair string

const (
	GateDesoUsdt currencyPair = "deso_usdt"
	GateUsdtUsd  currencyPair = "usdt_usd"
)

func getTickerResponseFromGate(currencyPair currencyPair) (*GateTickerResponse, error) {
	httpClient := &http.Client{}
	url := fmt.Sprintf("https://api.gateio.ws/api/v4/spot/tickers?currency_pair=%v", currencyPair)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		glog.Errorf("GetGateExchangeRate: Problem creating request: %v", err)
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		glog.Errorf("GetGateExchangeRate: Problem making request: %v", err)
		return nil, err
	}
	defer resp.Body.Close()
	// Decode the response into the appropriate struct.
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		glog.Errorf("GetGateExchangeRate: Problem reading response body: %v", err)
		return nil, err
	}
	responseData := []GateTickerResponse{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err = decoder.Decode(&responseData); err != nil {
		glog.Errorf("GetGateExchangeRate: Problem decoding response JSON into "+
			"interface %v, response: %v, error: %v", responseData, resp, err)
		return nil, err
	}
	if len(responseData) != 1 {
		return nil, fmt.Errorf("GetGateExchangeRate: unexpected number of tickers returned from Gate: %v", len(responseData))
	}
	return &responseData[0], nil
}

func (fes *APIServer) GetGateExchangeRate() (_exchangeRate float64, _err error) {
	desoToUSDTTickerResponse, err := getTickerResponseFromGate(GateDesoUsdt)
	if err != nil {
		glog.Errorf("GetGateExchangeRate: Problem fetching exchange rate from gate: %v", err)
		return 0, err
	}
	usdtToUSDTickerResponse, err := getTickerResponseFromGate(GateUsdtUsd)
	if err != nil {
		glog.Errorf("GetGateExchangeRate: Problem fetching exchange rate from gate: %v", err)
		return 0, err
	}
	usdtToUSDExchangePrice, err := strconv.ParseFloat(usdtToUSDTickerResponse.Last, 64)
	if err != nil {
		glog.Errorf("GetGateExchangeRate: Problem parsing USDT amount as float: %v", err)
		return 0, err
	}
	desoToUSDTExchangePrice, err := strconv.ParseFloat(desoToUSDTTickerResponse.Last, 64)
	if err != nil {
		glog.Errorf("GetGateExchangeRate: Problem parsing DESO amount as float: %v", err)
		return 0, err
	}

	// usdCents/DESO = (usdt/USD) * (DESO/USDT) * 100
	usdCentsToDESOExchangePrice := (usdtToUSDExchangePrice * desoToUSDTExchangePrice) * 100
	if fes.backendServer != nil && fes.backendServer.GetStatsdClient() != nil {
		if err = fes.backendServer.GetStatsdClient().Gauge("GATE_LAST_TRADE_PRICE", usdCentsToDESOExchangePrice, []string{}, 1); err != nil {
			glog.Errorf("GetGateExchangeRate: Error logging Last Trade Price of %f to datadog: %v", usdCentsToDESOExchangePrice, err)
		}
	}
	return usdCentsToDESOExchangePrice, nil
}

func (fes *APIServer) GetExchangeRateFromDeSoDex() (float64, error) {
	utxoView, err := fes.backendServer.GetMempool().GetAugmentedUniversalView()
	if err != nil {
		return 0, err
	}
	usdcProfileEntry := utxoView.GetProfileEntryForUsername([]byte(dusdcProfileUsername))
	if usdcProfileEntry == nil {
		return 0, fmt.Errorf("GetExchangeRateFromDeSoDex: Could not find profile entry for dusdc_")
	}

	usdcPKID := utxoView.GetPKIDForPublicKey(usdcProfileEntry.PublicKey)

	midPriceUSD, _, _, err := fes.GetHighestBidAndLowestAskPriceFromPKIDs(&lib.ZeroPKID, usdcPKID.PKID, utxoView, 0, false)
	if err != nil {
		return 0, err
	}
	return midPriceUSD * 100, nil
}

// UpdateUSDCentsToDeSoExchangeRate updates app state's USD Cents per DeSo value
func (fes *APIServer) UpdateUSDCentsToDeSoExchangeRate() {
	glog.V(2).Info("Refreshing exchange rate...")

	// Fetch price from blockchain.com
	blockchainDotComPrice, err := fes.GetBlockchainDotComExchangeRate()
	glog.V(2).Infof("Blockchain.com price (USD cents): %v", blockchainDotComPrice)
	if err != nil {
		glog.Errorf("UpdateUSDCentsToDeSoExchangeRate: Error fetching exchange rate from blockchain.com: %v", err)
	}

	// DEPRECATED: Fetch price from coinbase
	// coinbasePrice, err := fes.GetCoinbaseExchangeRate()
	// glog.V(2).Infof("Coinbase price (USD Cents): %v", coinbasePrice)
	// if err != nil {
	// 	 glog.Errorf("UpdateUSDCentsToDeSoExchangeRate: Error fetching exchange rate from coinbase: %v", err)
	// }

	// Fetch price from gate
	gatePrice, err := fes.GetGateExchangeRate()
	glog.V(2).Infof("Gate price (USD Cents): %v", gatePrice)
	if err != nil {
		glog.Errorf("UpdateUSDCentsToDeSoExchangeRate: Error fetching exchange rate from gate: %v", err)
	}

	desoDexPrice, err := fes.GetExchangeRateFromDeSoDex()
	glog.V(2).Infof("DeSoDex price (USD Cents): %v", desoDexPrice)
	if err != nil {
		glog.Errorf("UpdateUSDCentsToDeSoExchangeRate: Error fetching exchange rate from DeSoDex: %v", err)
	}

	// store the most recent exchange prices
	fes.MostRecentCoinbasePriceUSDCents = uint64(desoDexPrice)
	fes.MostRecentBlockchainDotComPriceUSDCents = uint64(blockchainDotComPrice)
	fes.MostRecentGatePriceUSDCents = uint64(gatePrice)
	fes.MostRecentDesoDexPriceUSDCents = uint64(desoDexPrice)

	// Get the current timestamp and append the current last trade price to the LastTradeDeSoPriceHistory slice
	timestamp := uint64(time.Now().UnixNano())
	fes.LastTradeDeSoPriceHistory = append(fes.LastTradeDeSoPriceHistory, LastTradePriceHistoryItem{
		LastTradePrice: uint64(desoDexPrice),
		Timestamp:      timestamp,
	})

	// Get the max price within the lookback window and remove elements that are no longer valid.
	maxPrice := fes.getMaxPriceFromHistoryAndCull(timestamp)

	// If the max of last trade price and 24H price is less than the reserve price, use the reserve price.
	if fes.USDCentsToDESOReserveExchangeRate > maxPrice {
		fes.UsdCentsPerDeSoExchangeRate = fes.USDCentsToDESOReserveExchangeRate
	} else {
		fes.UsdCentsPerDeSoExchangeRate = maxPrice
	}

	glog.V(2).Infof("Final exchange rate: %v", fes.UsdCentsPerDeSoExchangeRate)
}

func (fes *APIServer) UpdateUSDToBTCPrice() {
	glog.V(2).Info("Refreshing USD to BTC exchange rate")
	btcExchangeRate, err := GetUSDToBTCPrice()
	if err != nil {
		glog.Errorf("Error getting BTC price: %v", err)
		return
	}
	fes.UsdCentsPerBitCoinExchangeRate = btcExchangeRate * 100
	glog.V(2).Infof("New USD to BTC exchange rate: %f", fes.UsdCentsPerBitCoinExchangeRate/100)
}

func (fes *APIServer) UpdateUSDToETHPrice() {
	glog.V(2).Info("Refreshing USD to ETH exchange rate")
	ethExchangeRate, err := apis.GetUSDToETHPrice()
	if err != nil {
		glog.Errorf("Error getting ETH price: %v", err)
		return
	}
	fes.UsdCentsPerETHExchangeRate = uint64(ethExchangeRate * 100)
	glog.V(2).Infof("New USD to ETH exchange rate: %f", float64(fes.UsdCentsPerETHExchangeRate)/100)
}

// getMaxPriceFromHistoryAndCull removes elements that are outside of the lookback window and return the max price
// from valid elements.
func (fes *APIServer) getMaxPriceFromHistoryAndCull(currentTimestamp uint64) uint64 {
	maxPrice := uint64(0)
	// This function culls invalid values (outside of the lookback window) from the LastTradeDeSoPriceHistory slice
	// in place, so we need to keep track of the index at which we will place the next valid item.
	validIndex := 0
	for _, priceHistoryItem := range fes.LastTradeDeSoPriceHistory {
		tstampDiff := currentTimestamp - priceHistoryItem.Timestamp
		if tstampDiff <= fes.LastTradePriceLookback {
			// copy and increment index.  This overwrites invalid values with valid ones in the order valid items
			// are seen.
			fes.LastTradeDeSoPriceHistory[validIndex] = priceHistoryItem
			validIndex++
			if priceHistoryItem.LastTradePrice > maxPrice {
				maxPrice = priceHistoryItem.LastTradePrice
			}
		}
	}
	// Reduce the slice to only valid elements - all elements up to validIndex are within the lookback window.
	fes.LastTradeDeSoPriceHistory = fes.LastTradeDeSoPriceHistory[:validIndex]
	return maxPrice
}

type GetAppStateRequest struct {
	PublicKeyBase58Check string
}

type GetAppStateResponse struct {
	MinSatoshisBurnedForProfileCreation uint64
	BlockHeight                         uint32
	IsTestnet                           bool

	HasStarterDeSoSeed    bool
	HasTwilioAPIKey       bool
	CreateProfileFeeNanos uint64
	CompProfileCreation   bool
	DiamondLevelMap       map[int64]uint64
	HasWyreIntegration    bool
	HasJumioIntegration   bool
	BuyWithETH            bool

	USDCentsPerDeSoExchangeRate     uint64
	USDCentsPerDeSoCoinbase         uint64
	USDCentsPerDeSoBlockchainDotCom uint64
	JumioDeSoNanos                  uint64 // Deprecated
	JumioUSDCents                   uint64
	JumioKickbackUSDCents           uint64
	// CountrySignUpBonus is the sign-up bonus configuration for the country inferred from a request's IP address.
	CountrySignUpBonus CountryLevelSignUpBonus
	CaptchaDeSoNanos   uint64

	DefaultFeeRateNanosPerKB uint64
	TransactionFeeMap        map[string][]TransactionFee

	// Address to which we want to send ETH when used to buy DESO
	BuyETHAddress string

	Nodes map[uint64]lib.DeSoNode

	USDCentsPerBitCloutExchangeRate uint64 // Deprecated
	JumioBitCloutNanos              uint64 // Deprecated
}

func (fes *APIServer) GetAppState(ww http.ResponseWriter, req *http.Request) {
	decoder := json.NewDecoder(io.LimitReader(req.Body, MaxRequestBodySizeBytes))
	requestData := GetAppStateRequest{}
	if err := decoder.Decode(&requestData); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf(
			"GetAppState: Problem parsing request body: %v", err))
		return
	}

	// Get a view with all the mempool transactions (used to get all posts / reader state).
	utxoView, err := fes.backendServer.GetMempool().GetAugmentedUniversalView()
	if err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GetAppState: Error getting augmented universal view: %v", err))
		return
	}

	// Compute a default fee rate.
	globalParams := utxoView.GetCurrentGlobalParamsEntry()
	defaultFeeRateNanosPerKB := fes.MinFeeRateNanosPerKB
	if globalParams != nil && globalParams.MinimumNetworkFeeNanosPerKB > 0 {
		defaultFeeRateNanosPerKB = globalParams.MinimumNetworkFeeNanosPerKB
	}

	captchaDesoNanos, err := fes.getCaptchaRewardNanosFromGlobalState()
	if err != nil {
		captchaDesoNanos = 0
	}

	res := &GetAppStateResponse{
		MinSatoshisBurnedForProfileCreation: fes.Config.MinSatoshisForProfile,
		BlockHeight:                         fes.backendServer.GetBlockchain().BlockTip().Height,
		IsTestnet:                           fes.Params.NetworkType == lib.NetworkType_TESTNET,
		HasTwilioAPIKey:                     fes.Twilio != nil,
		HasStarterDeSoSeed:                  fes.Config.StarterDESOSeed != "",
		CreateProfileFeeNanos:               globalParams.CreateProfileFeeNanos,
		CompProfileCreation:                 fes.Config.CompProfileCreation,
		DiamondLevelMap:                     lib.GetDeSoNanosDiamondLevelMapAtBlockHeight(int64(fes.blockchain.BlockTip().Height)),
		HasWyreIntegration:                  fes.IsConfiguredForWyre(),
		HasJumioIntegration:                 fes.IsConfiguredForJumio(),
		BuyWithETH:                          fes.IsConfiguredForETH(),
		USDCentsPerDeSoExchangeRate:         fes.GetExchangeDeSoPrice(),
		USDCentsPerDeSoCoinbase:             fes.MostRecentCoinbasePriceUSDCents,
		USDCentsPerDeSoBlockchainDotCom:     fes.MostRecentBlockchainDotComPriceUSDCents,
		JumioDeSoNanos:                      fes.GetJumioDeSoNanos(), // Deprecated
		JumioUSDCents:                       fes.JumioUSDCents,
		JumioKickbackUSDCents:               fes.JumioKickbackUSDCents,
		CountrySignUpBonus:                  fes.GetCountryLevelSignUpBonusFromHeader(req),
		DefaultFeeRateNanosPerKB:            defaultFeeRateNanosPerKB,
		TransactionFeeMap:                   fes.TxnFeeMapToResponse(true),
		BuyETHAddress:                       fes.Config.BuyDESOETHAddress,
		Nodes:                               lib.NODES,
		CaptchaDeSoNanos:                    captchaDesoNanos,

		// Deprecated
		USDCentsPerBitCloutExchangeRate: fes.GetExchangeDeSoPrice(),
		JumioBitCloutNanos:              fes.GetJumioDeSoNanos(),
	}

	if err = json.NewEncoder(ww).Encode(res); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GetNotifications: Problem encoding response as JSON: %v", err))
		return
	}
}

type GetIngressCookieResponse struct {
	CookieValue string
}

// This route allows a client to get the cookie set by nginx for session affinity.
// This value can then be passed to a backend to ensure that all requests a user
// is making are being handled by the same machine.
func (fes *APIServer) GetIngressCookie(ww http.ResponseWriter, req *http.Request) {
	cookie, err := req.Cookie("INGRESSCOOKIE")
	if err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GetIngressCookie: Error getting ingress cookie: %v", err))
		return
	}
	if err = json.NewEncoder(ww).Encode(&GetIngressCookieResponse{CookieValue: cookie.Value}); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GetIngressCookie: Problem encoding response as JSON: %v", err))
		return
	}
}
