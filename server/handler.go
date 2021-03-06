package server

import (
	"context"
	"errors"
	"net"
	"sync/atomic"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/mitchellh/mapstructure"
	"go.blockdaemon.com/pyth"
	"go.blockdaemon.com/pythian/jsonrpc"
	"go.blockdaemon.com/pythian/schedule"
	"go.uber.org/zap"
)

const (
	rpcErrUnknownSymbol = -32000
	rpcErrNotReady      = -32002
)

type Handler struct {
	*jsonrpc.Mux
	Log       *zap.Logger
	client    *pyth.Client
	buffer    *schedule.Buffer
	publisher solana.PublicKey
	slots     *schedule.SlotMonitor
	subNonce  uint64
}

func NewHandler(
	client *pyth.Client,
	updateBuffer *schedule.Buffer,
	publisher solana.PublicKey,
	slots *schedule.SlotMonitor,
) *Handler {
	mux := jsonrpc.NewMux()
	h := &Handler{
		Mux:       mux,
		Log:       zap.NewNop(),
		client:    client,
		buffer:    updateBuffer,
		publisher: publisher,
		slots:     slots,
		subNonce:  1,
	}
	mux.HandleFunc("get_product_list", h.handleGetProductList)
	mux.HandleFunc("get_product", h.handleGetProduct)
	mux.HandleFunc("get_all_products", h.handleGetAllProducts)
	mux.HandleFunc("update_price", h.handleUpdatePrice)
	mux.HandleFunc("subscribe_price", h.handleSubscribePrice)
	mux.HandleFunc("subscribe_price_sched", h.handleSubscribePriceSchedule)
	return h
}

func (h *Handler) getAllProductsAndPrices(ctx context.Context) ([]pyth.ProductAccountEntry, map[solana.PublicKey][]pyth.PriceAccountEntry, error) {
	products, err := h.client.GetAllProductAccounts(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return nil, nil, err
	}
	priceKeys := make([]solana.PublicKey, 0, len(products))
	for _, product := range products {
		if !product.FirstPrice.IsZero() {
			priceKeys = append(priceKeys, product.FirstPrice)
		}
	}
	prices, err := h.client.GetPriceAccountsRecursive(ctx, rpc.CommitmentConfirmed, priceKeys...)
	if err != nil {
		return nil, nil, err
	}
	pricesPerProduct := make(map[solana.PublicKey][]pyth.PriceAccountEntry)
	for _, price := range prices {
		pricesPerProduct[price.Product] = append(pricesPerProduct[price.Product], price)
	}
	return products, pricesPerProduct, nil
}

func (h *Handler) handleGetProductList(ctx context.Context, req jsonrpc.Request, _ jsonrpc.Requester) *jsonrpc.Response {
	products, pricesPerProduct, err := h.getAllProductsAndPrices(ctx)
	if err != nil {
		return jsonrpc.NewErrorStringResponse(req.ID, rpcErrNotReady, "failed to get products: "+err.Error())
	}
	products2 := make([]productAccount, len(products))
	for i, prod := range products {
		products2[i] = productToJSON(prod, pricesPerProduct[prod.Pubkey])
	}
	return jsonrpc.NewResultResponse(req.ID, products2)
}

func (h *Handler) handleGetAllProducts(ctx context.Context, req jsonrpc.Request, _ jsonrpc.Requester) *jsonrpc.Response {
	products, pricesPerProduct, err := h.getAllProductsAndPrices(ctx)
	if err != nil {
		return jsonrpc.NewErrorStringResponse(req.ID, rpcErrNotReady, "failed to get products: "+err.Error())
	}
	products2 := make([]productAccountDetail, len(products))
	for i, prod := range products {
		products2[i] = productToDetailJSON(prod, pricesPerProduct[prod.Pubkey])
	}
	return jsonrpc.NewResultResponse(req.ID, products2)
}

func (h *Handler) handleGetProduct(ctx context.Context, req jsonrpc.Request, _ jsonrpc.Requester) *jsonrpc.Response {
	// Decode params.
	var params struct {
		Account solana.PublicKey `json:"account"`
	}
	if err := decodeParams(req.Params, &params); err != nil {
		return jsonrpc.NewInvalidParamsResponse(req.ID)
	}

	// Retrieve data from chain.
	entry, err := h.client.GetProductAccount(ctx, params.Account, rpc.CommitmentConfirmed)
	if errors.Is(err, rpc.ErrNotFound) {
		return jsonrpc.NewErrorStringResponse(req.ID, rpcErrUnknownSymbol, "unknown symbol")
	} else if err != nil {
		return jsonrpc.NewErrorStringResponse(req.ID, rpcErrNotReady, "failed to get product: "+err.Error())
	}
	prices, err := h.client.GetPriceAccountsRecursive(ctx, rpc.CommitmentConfirmed, entry.FirstPrice)
	if errors.Is(err, rpc.ErrNotFound) {
		return jsonrpc.NewErrorStringResponse(req.ID, rpcErrUnknownSymbol, "unknown symbol")
	} else if err != nil {
		return jsonrpc.NewErrorStringResponse(req.ID, rpcErrNotReady, "failed to get price accs: "+err.Error())
	}

	return jsonrpc.NewResultResponse(req.ID, productToDetailJSON(entry, prices))
}

func (h *Handler) handleUpdatePrice(_ context.Context, req jsonrpc.Request, _ jsonrpc.Requester) *jsonrpc.Response {
	// Decode params.
	var params struct {
		Account solana.PublicKey `json:"account"`
		Price   int64            `json:"price"`
		Conf    uint64           `json:"conf"`
		Status  string           `json:"status"`
	}
	if err := decodeParams(req.Params, &params); err != nil {
		return jsonrpc.NewInvalidParamsResponse(req.ID)
	}
	if params.Account.IsZero() || params.Price == 0 || params.Conf == 0 || params.Status == "" {
		return jsonrpc.NewInvalidParamsResponse(req.ID)
	}

	// Assemble instruction.
	update := pyth.CommandUpdPrice{
		Status:  statusFromString(params.Status),
		Price:   params.Price,
		Conf:    params.Conf,
		PubSlot: h.slots.Slot(),
	}
	ins := pyth.NewInstructionBuilder(h.client.Env.Program).
		UpdPriceNoFailOnError(h.publisher, params.Account, update)

	// Push instruction to write buffer. (Will be picked up by scheduler)
	h.buffer.PushUpdate(ins)

	return jsonrpc.NewResultResponse(req.ID, 0)
}

func (h *Handler) handleSubscribePrice(_ context.Context, req jsonrpc.Request, callback jsonrpc.Requester) *jsonrpc.Response {
	if req.ID == nil {
		return nil
	}

	// Decode params.
	var params struct {
		Account solana.PublicKey `json:"account"`
	}
	if err := decodeParams(req.Params, &params); err != nil {
		return jsonrpc.NewInvalidParamsResponse(req.ID)
	}
	if params.Account.IsZero() {
		return jsonrpc.NewInvalidParamsResponse(req.ID)
	}

	// Launch new subscription worker.
	subID := h.newSubID()
	go h.asyncSubscribePrice(params.Account, callback, subID)
	return newSubscriptionResponse(req.ID, subID)
}

func (h *Handler) asyncSubscribePrice(account solana.PublicKey, callback jsonrpc.Requester, subID uint64) {
	h.Log.Debug("Subscribing to price updates",
		zap.Stringer("program", h.client.Env.Program),
		zap.Stringer("price", account))
	defer h.Log.Debug("Unsubscribing from price updates",
		zap.Stringer("price", account))

	// TODO(richard): This is inefficient, no need to stream copy of all price updates for _each_ subscription.
	stream := h.client.StreamPriceAccounts()
	defer stream.Close()

	handler := pyth.NewPriceEventHandler(stream)
	handler.OnPriceChange(account, func(update pyth.PriceUpdate) {
		price := priceUpdate{
			Price:     update.CurrentInfo.Price,
			Conf:      update.CurrentInfo.Conf,
			Status:    statusToString(update.CurrentInfo.Status),
			ValidSlot: update.Account.ValidSlot,
			PubSlot:   update.CurrentInfo.PubSlot,
		}
		err := callback.AsyncRequestJSONRPC(context.Background(), "notify_price", subscriptionUpdate{
			Result:       &price,
			Subscription: subID,
		})
		if err != nil {
			h.Log.Warn("Failed to deliver async price update", zap.Error(err))
		}
	})

	<-callback.Done()
}

func (h *Handler) handleSubscribePriceSchedule(_ context.Context, req jsonrpc.Request, callback jsonrpc.Requester) *jsonrpc.Response {
	if req.ID == nil {
		return nil
	}

	// Decode params.
	var params struct {
		Account solana.PublicKey `json:"account"`
	}
	if err := decodeParams(req.Params, &params); err != nil {
		return jsonrpc.NewInvalidParamsResponse(req.ID)
	}
	if params.Account.IsZero() {
		return jsonrpc.NewInvalidParamsResponse(req.ID)
	}

	// Launch new subscription worker.
	subID := h.newSubID()
	go h.asyncSubscribePriceSchedule(callback, subID)
	return newSubscriptionResponse(req.ID, subID)
}

func (h *Handler) asyncSubscribePriceSchedule(callback jsonrpc.Requester, subID uint64) {
	var unsub context.CancelFunc
	unsub = h.slots.Subscribe(func(slot uint64) {
		err := callback.AsyncRequestJSONRPC(context.Background(), "notify_price_sched", subscriptionUpdate{
			Subscription: subID,
		})
		if errors.Is(err, net.ErrClosed) {
			go unsub()
		} else if err != nil {
			h.Log.Warn("Failed to deliver async price schedule update", zap.Error(err))
		}
	})
}

func newSubscriptionResponse(reqID interface{}, subID uint64) *jsonrpc.Response {
	var result struct {
		Subscription uint64 `json:"subscription"`
	}
	result.Subscription = subID
	return jsonrpc.NewResultResponse(reqID, &result)
}

func (h *Handler) newSubID() uint64 {
	return atomic.AddUint64(&h.subNonce, 1)
}

func decodeParams(params interface{}, out interface{}) error {
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook: mapstructure.TextUnmarshallerHookFunc(),
		Result:     out,
	})
	if err != nil {
		return err
	}
	return dec.Decode(params)
}
