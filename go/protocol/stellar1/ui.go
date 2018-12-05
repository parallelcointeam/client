// Auto-generated by avdl-compiler v1.3.28 (https://github.com/keybase/node-avdl-compiler)
//   Input file: avdl/stellar1/ui.avdl

package stellar1

import (
	"github.com/keybase/go-framed-msgpack-rpc/rpc"
	context "golang.org/x/net/context"
)

type UIPaymentReview struct {
	Bid        BuildPaymentID    `codec:"bid" json:"bid"`
	Seqno      int               `codec:"seqno" json:"seqno"`
	Banners    []SendBannerLocal `codec:"banners" json:"banners"`
	NextButton string            `codec:"nextButton" json:"nextButton"`
}

func (o UIPaymentReview) DeepCopy() UIPaymentReview {
	return UIPaymentReview{
		Bid:   o.Bid.DeepCopy(),
		Seqno: o.Seqno,
		Banners: (func(x []SendBannerLocal) []SendBannerLocal {
			if x == nil {
				return nil
			}
			ret := make([]SendBannerLocal, len(x))
			for i, v := range x {
				vCopy := v.DeepCopy()
				ret[i] = vCopy
			}
			return ret
		})(o.Banners),
		NextButton: o.NextButton,
	}
}

type UiPaymentReviewArg struct {
	SessionID int             `codec:"sessionID" json:"sessionID"`
	Msg       UIPaymentReview `codec:"msg" json:"msg"`
}

type UiInterface interface {
	UiPaymentReview(context.Context, UiPaymentReviewArg) error
}

func UiProtocol(i UiInterface) rpc.Protocol {
	return rpc.Protocol{
		Name: "stellar.1.ui",
		Methods: map[string]rpc.ServeHandlerDescription{
			"uiPaymentReview": {
				MakeArg: func() interface{} {
					var ret [1]UiPaymentReviewArg
					return &ret
				},
				Handler: func(ctx context.Context, args interface{}) (ret interface{}, err error) {
					typedArgs, ok := args.(*[1]UiPaymentReviewArg)
					if !ok {
						err = rpc.NewTypeError((*[1]UiPaymentReviewArg)(nil), args)
						return
					}
					err = i.UiPaymentReview(ctx, typedArgs[0])
					return
				},
			},
		},
	}
}

type UiClient struct {
	Cli rpc.GenericClient
}

func (c UiClient) UiPaymentReview(ctx context.Context, __arg UiPaymentReviewArg) (err error) {
	err = c.Cli.Call(ctx, "stellar.1.ui.uiPaymentReview", []interface{}{__arg}, nil)
	return
}
