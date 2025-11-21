package trader

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"nofx/market"
)

const defaultSimulatedFeeRate = 0.0004

// simulatedPosition represents an in-memory position for paper trading.
type simulatedPosition struct {
	Symbol      string
	Side        string // "long" or "short"
	Quantity    float64
	EntryPrice  float64
	Leverage    int
	MarginUsed  float64
	StopLoss    float64
	TakeProfit  float64
	MarginMode  bool // true=cross, false=isolated (kept for compatibility logging)
	Initialized bool
}

// SimulatedTrader implements Trader interface without touching real exchanges.
type SimulatedTrader struct {
	mu sync.Mutex

	walletBalance    float64
	availableBalance float64
	feeRate          float64
	isCrossMargin    bool

	positions    map[string]*simulatedPosition // key = symbol + "_" + side
	orderCounter int64
}

// NewSimulatedTrader creates a paper-trading exchange adapter.
func NewSimulatedTrader(initialBalance float64, isCrossMargin bool) *SimulatedTrader {
	return &SimulatedTrader{
		walletBalance:    initialBalance,
		availableBalance: initialBalance,
		feeRate:          defaultSimulatedFeeRate,
		isCrossMargin:    isCrossMargin,
		positions:        make(map[string]*simulatedPosition),
	}
}

func (st *SimulatedTrader) nextOrderID() int64 {
	st.orderCounter++
	return st.orderCounter
}

func (st *SimulatedTrader) positionKey(symbol, side string) string {
	return fmt.Sprintf("%s_%s", symbol, side)
}

// clonePositions returns a shallow copy of current positions for read-only ops.
func (st *SimulatedTrader) snapshot() ([]*simulatedPosition, float64, float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	list := make([]*simulatedPosition, 0, len(st.positions))
	for _, p := range st.positions {
		cp := *p
		list = append(list, &cp)
	}
	return list, st.walletBalance, st.availableBalance
}

// GetBalance returns simulated account balances.
func (st *SimulatedTrader) GetBalance() (map[string]interface{}, error) {
	positions, wallet, available := st.snapshot()

	totalUnrealized := 0.0
	for _, pos := range positions {
		price, err := marketPrice(pos.Symbol)
		if err != nil {
			continue
		}
		unrealized := unrealizedPnL(pos.Side, pos.EntryPrice, price, pos.Quantity)
		totalUnrealized += unrealized
	}

	return map[string]interface{}{
		"totalWalletBalance":    wallet,
		"wallet_balance":        wallet,
		"balance":               wallet,
		"availableBalance":      available,
		"available_margin":      available,
		"totalUnrealizedProfit": totalUnrealized,
	}, nil
}

// GetPositions returns all simulated positions.
func (st *SimulatedTrader) GetPositions() ([]map[string]interface{}, error) {
	positions, wallet, available := st.snapshot()
	result := make([]map[string]interface{}, 0, len(positions))

	for _, pos := range positions {
		price, err := marketPrice(pos.Symbol)
		if err != nil {
			continue
		}

		unrealized := unrealizedPnL(pos.Side, pos.EntryPrice, price, pos.Quantity)
		marginUsed := pos.MarginUsed
		entry := pos.EntryPrice
		lev := float64(maxInt(pos.Leverage, 1))

		liqPrice := calculateLiquidationPrice(pos.Side, entry, lev)
		quantity := pos.Quantity
		if pos.Side == "short" {
			quantity = -quantity
		}

		marginType := "isolated"
		if pos.MarginMode {
			marginType = "cross"
		}

		crossWalletBalance := marginUsed
		if pos.MarginMode {
			crossWalletBalance = wallet
		}

		result = append(result, map[string]interface{}{
			"symbol":            pos.Symbol,
			"side":              pos.Side,
			"positionSide":      strings.ToUpper(pos.Side),
			"positionAmt":       quantity,
			"entryPrice":        entry,
			"leverage":          float64(pos.Leverage),
			"markPrice":         price,
			"unRealizedProfit":  unrealized,
			"liquidationPrice":  liqPrice,
			"marginType":        marginType,
			"isolatedMargin":    marginUsed,
			"notionalValue":     price * math.Abs(quantity),
			"updateTime":        0,
			"unrealizedProfit":  unrealized,
			"positionMargin":    marginUsed,
			"initialMargin":     marginUsed,
			"maintMargin":       marginUsed / lev,
			"marginRatio":       marginUsed / st.walletBalance,
			"positionCost":      entry * math.Abs(quantity),
			"stopLoss":          pos.StopLoss,
			"takeProfit":        pos.TakeProfit,
			"isolatedWallet":    marginUsed,
			"maxNotionalValue":  0.0,
			"availableBalance":  available,
			"crossWalletBalance": crossWalletBalance,
		})
	}

	return result, nil
}

// OpenLong opens a simulated long position.
func (st *SimulatedTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return st.openPosition(symbol, quantity, leverage, "long")
}

// OpenShort opens a simulated short position.
func (st *SimulatedTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return st.openPosition(symbol, quantity, leverage, "short")
}

func (st *SimulatedTrader) openPosition(symbol string, quantity float64, leverage int, side string) (map[string]interface{}, error) {
	if quantity <= 0 {
		return nil, fmt.Errorf("quantity must be positive")
	}
	if leverage <= 0 {
		leverage = 1
	}

	price, err := marketPrice(symbol)
	if err != nil {
		return nil, err
	}

	notional := price * quantity
	marginRequired := notional / float64(leverage)
	fee := notional * st.feeRate

	key := st.positionKey(symbol, side)

	st.mu.Lock()
	defer st.mu.Unlock()

	if _, exists := st.positions[key]; exists {
		return nil, fmt.Errorf("%s already has an open %s position", symbol, side)
	}

	totalDeduction := marginRequired + fee
	if st.availableBalance < totalDeduction {
		return nil, fmt.Errorf("insufficient available balance: need %.4f, available %.4f", totalDeduction, st.availableBalance)
	}

	st.availableBalance -= totalDeduction
	st.walletBalance -= fee

	st.positions[key] = &simulatedPosition{
		Symbol:      symbol,
		Side:        side,
		Quantity:    quantity,
		EntryPrice:  price,
		Leverage:    leverage,
		MarginUsed:  marginRequired,
		MarginMode:  st.isCrossMargin,
		Initialized: true,
	}

	orderID := st.nextOrderID()
	return map[string]interface{}{
		"orderId": orderID,
		"symbol":  symbol,
		"status":  "FILLED",
		"avgPrice": func() float64 {
			return price
		}(),
	}, nil
}

// CloseLong closes a simulated long position.
func (st *SimulatedTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	return st.closePosition(symbol, quantity, "long")
}

// CloseShort closes a simulated short position.
func (st *SimulatedTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	return st.closePosition(symbol, quantity, "short")
}

func (st *SimulatedTrader) closePosition(symbol string, quantity float64, side string) (map[string]interface{}, error) {
	price, err := marketPrice(symbol)
	if err != nil {
		return nil, err
	}

	key := st.positionKey(symbol, side)

	st.mu.Lock()
	defer st.mu.Unlock()

	pos, exists := st.positions[key]
	if !exists {
		return nil, fmt.Errorf("no open %s position for %s", side, symbol)
	}

	closeQty := pos.Quantity
	if quantity > 0 && quantity < pos.Quantity {
		closeQty = quantity
	}

	if closeQty <= 0 {
		return nil, fmt.Errorf("close quantity must be positive")
	}

	proportion := closeQty / pos.Quantity
	marginRelease := pos.MarginUsed * proportion
	fee := price * closeQty * st.feeRate

	var pnl float64
	if side == "long" {
		pnl = (price - pos.EntryPrice) * closeQty
	} else {
		pnl = (pos.EntryPrice - price) * closeQty
	}

	st.availableBalance += marginRelease + pnl - fee
	st.walletBalance += pnl - fee

	if closeQty == pos.Quantity {
		delete(st.positions, key)
	} else {
		pos.Quantity -= closeQty
		pos.MarginUsed -= marginRelease
	}

	orderID := st.nextOrderID()
	return map[string]interface{}{
		"orderId": orderID,
		"symbol":  symbol,
		"status":  "FILLED",
		"avgPrice": func() float64 {
			return price
		}(),
	}, nil
}

// SetLeverage stores leverage preference for future reference.
func (st *SimulatedTrader) SetLeverage(symbol string, leverage int) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	if leverage <= 0 {
		return fmt.Errorf("leverage must be positive")
	}

	for _, side := range []string{"long", "short"} {
		if pos, ok := st.positions[st.positionKey(symbol, side)]; ok {
			pos.Leverage = leverage
		}
	}

	return nil
}

// SetMarginMode switches between cross and isolated margin (metadata only).
func (st *SimulatedTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.isCrossMargin = isCrossMargin

	for _, side := range []string{"long", "short"} {
		if pos, ok := st.positions[st.positionKey(symbol, side)]; ok {
			pos.MarginMode = isCrossMargin
		}
	}
	return nil
}

// GetMarketPrice returns the latest simulated price using real market data.
func (st *SimulatedTrader) GetMarketPrice(symbol string) (float64, error) {
	return marketPrice(symbol)
}

// SetStopLoss records stop-loss preference (no automatic trigger in simulation).
func (st *SimulatedTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	side := normalizeSide(positionSide)
	if pos, ok := st.positions[st.positionKey(symbol, side)]; ok {
		pos.StopLoss = stopPrice
	}
	return nil
}

// SetTakeProfit records take-profit preference.
func (st *SimulatedTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	side := normalizeSide(positionSide)
	if pos, ok := st.positions[st.positionKey(symbol, side)]; ok {
		pos.TakeProfit = takeProfitPrice
	}
	return nil
}

// CancelStopLossOrders clears stored stop-loss values.
func (st *SimulatedTrader) CancelStopLossOrders(symbol string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, side := range []string{"long", "short"} {
		if pos, ok := st.positions[st.positionKey(symbol, side)]; ok {
			pos.StopLoss = 0
		}
	}
	return nil
}

// CancelTakeProfitOrders clears stored take-profit values.
func (st *SimulatedTrader) CancelTakeProfitOrders(symbol string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, side := range []string{"long", "short"} {
		if pos, ok := st.positions[st.positionKey(symbol, side)]; ok {
			pos.TakeProfit = 0
		}
	}
	return nil
}

// CancelAllOrders clears simulated stop orders.
func (st *SimulatedTrader) CancelAllOrders(symbol string) error {
	_ = st.CancelStopOrders(symbol)
	return nil
}

// CancelStopOrders clears both stop-loss and take-profit preferences.
func (st *SimulatedTrader) CancelStopOrders(symbol string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, side := range []string{"long", "short"} {
		if pos, ok := st.positions[st.positionKey(symbol, side)]; ok {
			pos.StopLoss = 0
			pos.TakeProfit = 0
		}
	}
	return nil
}

// FormatQuantity formats quantity to 4 decimal places, matching typical exchange precision.
func (st *SimulatedTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	return fmt.Sprintf("%.4f", quantity), nil
}

// Helpers

func marketPrice(symbol string) (float64, error) {
	data, err := market.Get(symbol)
	if err != nil {
		return 0, err
	}
	if data == nil || data.CurrentPrice <= 0 {
		return 0, fmt.Errorf("no market data for %s", symbol)
	}
	return data.CurrentPrice, nil
}

func unrealizedPnL(side string, entry, mark, quantity float64) float64 {
	switch side {
	case "long":
		return (mark - entry) * quantity
	case "short":
		return (entry - mark) * quantity
	default:
		return 0
	}
}

func calculateLiquidationPrice(side string, entry float64, leverage float64) float64 {
	if leverage <= 0 {
		return 0
	}
	switch side {
	case "long":
		return entry * (1 - 1/leverage)
	case "short":
		return entry * (1 + 1/leverage)
	default:
		return 0
	}
}

func normalizeSide(positionSide string) string {
	switch strings.ToUpper(positionSide) {
	case "LONG":
		return "long"
	case "SHORT":
		return "short"
	default:
		return strings.ToLower(positionSide)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

