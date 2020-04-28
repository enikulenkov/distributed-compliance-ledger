package rest

import (
	"encoding/json"
	"fmt"
	authutils "git.dsr-corporation.com/zb-ledger/zb-ledger/utils/auth"
	"git.dsr-corporation.com/zb-ledger/zb-ledger/utils/pagination"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/context"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/rest"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/auth/client/utils"
	"github.com/gorilla/mux"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	"net/http"
	"strconv"
)

const (
	FlagPreviousHeight = "prev_height" // Query data from previous height to avoid delay linked to state proof verification
)

type RestContext struct {
	context        client.CLIContext
	responseWriter http.ResponseWriter
	request        *http.Request
	baseReq        rest.BaseReq
	signer         sdk.AccAddress
}

func NewRestContext(w http.ResponseWriter, r *http.Request) RestContext {
	return RestContext{
		context:        context.NewCLIContext(),
		responseWriter: w,
		request:        r,
	}
}

func (ctx RestContext) Codec() *codec.Codec {
	return ctx.context.Codec
}

func (ctx RestContext) Variables() map[string]string {
	return mux.Vars(ctx.request)
}

func (ctx RestContext) Request() *http.Request {
	return ctx.request
}

func (ctx RestContext) Signer() sdk.AccAddress {
	return ctx.signer
}

func (ctx RestContext) BlockchainInfo(minHeight, maxHeight int64) (*ctypes.ResultBlockchainInfo, error) {
	return ctx.context.Client.BlockchainInfo(minHeight, maxHeight)
}

func (ctx RestContext) ResponseWriter() *http.ResponseWriter {
	return &ctx.responseWriter
}

func (ctx RestContext) WithCodec(cdc *codec.Codec) RestContext {
	ctx.context = ctx.context.WithCodec(cdc)
	return ctx
}

func (ctx RestContext) WithResponseWriter(w http.ResponseWriter) RestContext {
	ctx.responseWriter = w
	return ctx
}

func (ctx RestContext) WithHeight(height int64) RestContext {
	ctx.context = ctx.context.WithHeight(height)
	return ctx
}

func (ctx RestContext) WithFormerHeight() (RestContext, error) {
	node, err := ctx.context.GetNode()
	if err != nil {
		rest.WriteErrorResponse(ctx.responseWriter, http.StatusInternalServerError, err.Error())
		return RestContext{}, err
	}

	status, err := node.Status()
	if err != nil {
		rest.WriteErrorResponse(ctx.responseWriter, http.StatusInternalServerError, err.Error())
		return RestContext{}, err
	}

	ctx.context = ctx.context.WithHeight(status.SyncInfo.LatestBlockHeight - 1)
	return ctx, nil
}

func (ctx RestContext) WithSigner() (RestContext, error) {
	from, err := sdk.AccAddressFromBech32(ctx.baseReq.From)
	if err != nil {
		rest.WriteErrorResponse(ctx.responseWriter, http.StatusBadRequest, fmt.Sprintf("Request Parsing Error: %v. `from` must be a valid address", err))
		return RestContext{}, err
	}
	ctx.signer = from
	return ctx, nil
}

func (ctx RestContext) WithBaseRequest(baseReq rest.BaseReq) (RestContext, error) {
	ctx.baseReq = baseReq.Sanitize()
	if !baseReq.ValidateBasic(ctx.responseWriter) {
		return RestContext{}, sdk.ErrInternal("Base request validation failed")
	}
	return ctx, nil
}

func (ctx RestContext) ReadRESTReq(req interface{}) bool {
	return rest.ReadRESTReq(ctx.responseWriter, ctx.request, ctx.Codec(), req)
}

func (ctx RestContext) QueryStore(key string, storeName string) ([]byte, int64, error) {
	requestPrevState := false
	var err error

	if flag := ctx.request.FormValue(FlagPreviousHeight); len(flag) > 0 {
		requestPrevState, err = strconv.ParseBool(flag)
		if err != nil {
			return nil, 0, err
		}
	}

	if requestPrevState { // Try to query row on `height-1` to avoid delay related to waiting of committing block with height + 1
		ctx, err := ctx.WithFormerHeight()
		if err != nil {
			return nil, 0, err
		}

		res, height, err := ctx.context.QueryStore([]byte(key), storeName)
		if res != nil {
			return res, height, err
		}
	}
	// request on the current height
	ctx.context = ctx.context.WithHeight(0)
	return ctx.context.QueryStore([]byte(key), storeName)
}

func (ctx RestContext) QueryWithData(path string, data interface{}) ([]byte, int64, error) {
	return ctx.context.QueryWithData(path, ctx.context.Codec.MustMarshalJSON(data))
}

func (ctx RestContext) QueryList(path string, params interface{}) {
	res, height, err := ctx.QueryWithData(path, params)
	if err != nil {
		rest.WriteErrorResponse(ctx.responseWriter, http.StatusNotFound, err.Error())
		return
	}

	ctx.RespondWithHeight(res, height)
}

func (ctx RestContext) EncodeAndRespondWithHeight(data interface{}, height int64) {
	out, err := json.Marshal(data)
	if err != nil {
		rest.WriteErrorResponse(ctx.responseWriter, http.StatusInternalServerError, err.Error())
		return
	}

	ctx.RespondWithHeight(out, height)
}

func (ctx RestContext) ParsePaginationParams() (pagination.PaginationParams, error) {
	paginationParams, err := pagination.ParsePaginationParamsFromRequest(ctx.request)
	if err != nil {
		rest.WriteErrorResponse(ctx.responseWriter, http.StatusBadRequest, err.Error())
		return pagination.PaginationParams{}, err
	}
	return paginationParams, nil
}

func (ctx RestContext) PostProcessResponseBare(body interface{}) {
	rest.PostProcessResponseBare(ctx.responseWriter, ctx.context, body)
}

func (ctx RestContext) PostProcessResponse(body interface{}) {
	rest.PostProcessResponse(ctx.responseWriter, ctx.context, body)
}

func (ctx RestContext) HandleWriteRequest(msg sdk.Msg) {
	err := msg.ValidateBasic()
	if err != nil {
		ctx.WriteErrorResponse(http.StatusBadRequest, err.Error())
		return
	}

	account, passphrase, err_ := authutils.GetCredentialsFromRequest(ctx.request)
	if err_ != nil { // No credentials - just generate request message
		utils.WriteGenerateStdTxResponse(ctx.responseWriter, ctx.context, ctx.baseReq, []sdk.Msg{msg})
		return
	}

	// Credentials are found - sign and broadcast message
	res, err_ := ctx.SignAndBroadcastMessage(ctx.baseReq.ChainID, account, passphrase, []sdk.Msg{msg})
	if err_ != nil {
		rest.WriteErrorResponse(ctx.responseWriter, http.StatusInternalServerError, err_.Error())
		return
	}

	rest.PostProcessResponse(ctx.responseWriter, ctx.context, res)
}

func (ctx RestContext) RespondWithHeight(out interface{}, height int64) {
	ctx.context = ctx.context.WithHeight(height)
	rest.PostProcessResponse(ctx.responseWriter, ctx.context, out)
}

func (ctx RestContext) WriteErrorResponse(status int, err string) {
	rest.WriteErrorResponse(ctx.responseWriter, status, err)
}

func (ctx RestContext) SignMessage(chainId string, name string, passphrase string, msg []sdk.Msg) ([]byte, error) {
	acc, err := auth.NewAccountRetriever(ctx.context).GetAccount(ctx.signer)
	if err != nil {
		return nil, err
	}

	txBldr := auth.NewTxBuilderFromCLI().
		WithTxEncoder(utils.GetTxEncoder(ctx.Codec())).
		WithAccountNumber(acc.GetAccountNumber()).
		WithSequence(acc.GetSequence()).
		WithChainID(chainId)

	return txBldr.BuildAndSign(name, passphrase, msg)
}

func (ctx RestContext) BroadcastMessage(message []byte) ([]byte, error) {
	ctx.context.BroadcastMode = "block"
	res, err := ctx.context.BroadcastTx(message)
	if err != nil {
		return nil, err
	}

	txBytes, err := ctx.Codec().MarshalJSON(res)
	if err != nil {
		return nil, err
	}

	return txBytes, nil
}

func (ctx RestContext) SignAndBroadcastMessage(chainId string, account string, passphrase string, msg []sdk.Msg) ([]byte, error) {
	signedMsg, err := ctx.SignMessage(chainId, account, passphrase, msg)
	if err != nil {
		return nil, err
	}

	return ctx.BroadcastMessage(signedMsg)
}