package stellar

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/keybase/client/go/engine"
	"github.com/keybase/client/go/externals"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/client/go/protocol/stellar1"
	"github.com/keybase/client/go/stellar/acctbundle"
	"github.com/keybase/client/go/stellar/relays"
	"github.com/keybase/client/go/stellar/remote"
	"github.com/keybase/client/go/stellar/stellarcommon"
	"github.com/keybase/stellarnet"
	stellarAddress "github.com/stellar/go/address"
	"github.com/stellar/go/build"
)

const AccountNameMaxRunes = 24

// CreateWallet creates and posts an initial stellar bundle for a user.
// Only succeeds if they do not already have one.
// Safe (but wasteful) to call even if the user has a bundle already.
func CreateWallet(ctx context.Context, g *libkb.GlobalContext, v2Link bool) (created bool, err error) {
	defer g.CTraceTimed(ctx, "Stellar.CreateWallet", func() error { return err })()
	loggedInUsername := g.ActiveDevice.Username(libkb.NewMetaContext(ctx, g))
	if !loggedInUsername.IsValid() {
		return false, fmt.Errorf("could not get logged-in username")
	}
	perUserKeyUpgradeSoft(ctx, g, "create-wallet")
	clearBundle, err := acctbundle.NewInitial(fmt.Sprintf("%v's account", loggedInUsername))
	if err != nil {
		return false, err
	}
	meUV, err := g.GetMeUV(ctx)
	if err != nil {
		return false, err
	}
	err = remote.PostWithChainlink(ctx, g, *clearBundle, v2Link)
	switch e := err.(type) {
	case nil:
		// ok
	case libkb.AppStatusError:
		switch keybase1.StatusCode(e.Code) {
		case keybase1.StatusCode_SCStellarWrongRevision:
			// Assume this happened because a bundle already existed.
			// And suppress the error.
			g.Log.CDebugf(ctx, "suppressing error: %v", err)
			return false, nil
		}
		return false, err
	default:
		return false, err
	}
	getGlobal(g).InformHasWallet(ctx, meUV)
	go getGlobal(g).KickAutoClaimRunner(libkb.NewMetaContext(ctx, g), gregor1.MsgID{})
	return true, nil
}

type CreateWalletGatedResult struct {
	JustCreated        bool // whether the user's wallet was created by this call
	HasWallet          bool // whether the user now has a wallet
	AcceptedDisclaimer bool // whether the user has accepted the disclaimer
}

// CreateWalletGated may create a wallet for the user.
// Taking into account settings from the server.
// It should be speedy to call repeatedly _if_ the user gets a wallet.
func CreateWalletGated(ctx context.Context, g *libkb.GlobalContext) (res CreateWalletGatedResult, err error) {
	defer g.CTraceTimed(ctx, "Stellar.CreateWalletGated", func() error { return err })()
	defer func() {
		g.Log.CDebugf(ctx, "CreateWalletGated: (res:%+v, err:%v)", res, err != nil)
	}()
	meUV, err := g.GetMeUV(ctx)
	if err != nil {
		return res, err
	}
	if getGlobal(g).CachedHasWallet(ctx, meUV) {
		g.Log.CDebugf(ctx, "CreateWalletGated: local cache says we already have a wallet")
		return CreateWalletGatedResult{
			JustCreated:        false,
			HasWallet:          true,
			AcceptedDisclaimer: true, // because it should be impossible to have created a wallet without accepting.
		}, nil
	}
	scr, err := remote.ShouldCreate(ctx, g)
	if err != nil {
		return res, err
	}
	res.HasWallet = scr.HasWallet
	res.AcceptedDisclaimer = scr.AcceptedDisclaimer
	if scr.HasWallet {
		g.Log.CDebugf(ctx, "CreateWalletGated: server says we already have a wallet")
		getGlobal(g).InformHasWallet(ctx, meUV)
		return res, nil
	}
	if !scr.ShouldCreate {
		g.Log.CDebugf(ctx, "CreateWalletGated: server did not recommend wallet creation")
		return res, nil
	}
	justCreated, err := CreateWallet(ctx, g, false)
	if err != nil {
		return res, nil
	}
	res.JustCreated = justCreated
	if justCreated {
		res.HasWallet = true
	}
	return res, nil
}

// CreateWalletSoft creates a user's initial wallet if they don't already have one.
// Does not get in the way of intentional user actions.
func CreateWalletSoft(ctx context.Context, g *libkb.GlobalContext) {
	var err error
	defer g.CTraceTimed(ctx, "CreateWalletSoft", func() error { return err })()
	if !g.LocalSigchainGuard().IsAvailable(ctx, "CreateWalletSoft") {
		err = fmt.Errorf("yielding to guard")
		return
	}
	_, err = CreateWalletGated(ctx, g)
}

// Upkeep makes sure the bundle is encrypted for the user's latest PUK.
func Upkeep(ctx context.Context, g *libkb.GlobalContext) (err error) {
	defer g.CTraceTimed(ctx, "Stellar.Upkeep", func() error { return err })()
	prevBundle, version, prevPukGen, err := remote.FetchWholeBundle(ctx, g)
	if err != nil {
		return err
	}
	pukring, err := g.GetPerUserKeyring(ctx)
	if err != nil {
		return err
	}
	err = pukring.Sync(libkb.NewMetaContext(ctx, g))
	if err != nil {
		return err
	}
	pukGen := pukring.CurrentGeneration()
	if pukGen <= prevPukGen {
		g.Log.CDebugf(ctx, "Stellar.Upkeep: early out prevPukGen:%v < pukGen:%v", prevPukGen, pukGen)
		return nil
	}
	nextBundle := acctbundle.AdvanceAll(*prevBundle)
	return remote.Post(ctx, g, nextBundle, version)
}

func ImportSecretKey(ctx context.Context, g *libkb.GlobalContext, secretKey stellar1.SecretKey, makePrimary bool, accountName string) (err error) {
	prevBundle, version, _, err := remote.FetchSecretlessBundle(ctx, g)
	if err != nil {
		return err
	}
	var v2Link bool
	if version == stellar1.BundleVersion_V1 {
		prevBundle, _, _, err = remote.FetchWholeBundle(ctx, g)
		if err != nil {
			return err
		}
		v2Link = false
	} else {
		v2Link = true
	}

	nextBundle := acctbundle.AdvanceBundle(*prevBundle)
	err = acctbundle.AddAccount(&nextBundle, secretKey, accountName, makePrimary)
	if err != nil {
		return err
	}

	if makePrimary {
		// primary account changes need sigchain link
		// (so other users can find user's primary account id)
		err = remote.PostWithChainlink(ctx, g, nextBundle, v2Link)
	} else {
		err = remote.Post(ctx, g, nextBundle, version)
	}
	if err != nil {
		return err
	}

	// after import, mark all the transactions in this account as "read"
	// any errors in this process are not fatal, since the important task
	// has been accomplished.
	_, accountID, _, err := libkb.ParseStellarSecretKey(string(secretKey))
	if err != nil {
		g.Log.CDebugf(ctx, "ImportSecretKey, failed to parse secret key after import: %s", err)
		return nil
	}
	page, err := remote.RecentPayments(ctx, g, accountID, nil, 0, true)
	if err != nil {
		g.Log.CDebugf(ctx, "ImportSecretKey, RecentPayments error: %s", err)
		return nil
	}
	if len(page.Payments) == 0 {
		return nil
	}
	mostRecentID, err := page.Payments[0].TransactionID()
	if err != nil {
		g.Log.CDebugf(ctx, "ImportSecretKey, tx id from most recent payment error: %s", err)
		return nil
	}
	if err = remote.MarkAsRead(ctx, g, accountID, mostRecentID); err != nil {
		g.Log.CDebugf(ctx, "ImportSecretKey, markAsRead error: %s", err)
		return nil
	}

	return nil
}

func EnableMigrationFeatureFlag(ctx context.Context, g *libkb.GlobalContext) error {
	if g.GetRunMode() == libkb.ProductionRunMode {
		return errors.New("this doesn't work in production")
	}
	m := libkb.NewMetaContext(ctx, g)
	_, err := g.API.Post(libkb.APIArg{
		Endpoint:    "test/feature",
		SessionType: libkb.APISessionTypeREQUIRED,
		Args: libkb.HTTPArgs{
			"feature":   libkb.S{Val: string(libkb.FeatureStellarAcctBundles)},
			"value":     libkb.I{Val: 1},
			"cache_sec": libkb.I{Val: 100},
		},
		MetaContext: m,
	})
	return err
}

func ExportSecretKey(ctx context.Context, g *libkb.GlobalContext, accountID stellar1.AccountID) (res stellar1.SecretKey, err error) {
	prevBundle, _, _, err := remote.FetchAccountBundle(ctx, g, accountID)
	if err != nil {
		return res, err
	}
	for _, account := range prevBundle.Accounts {
		if account.AccountID.Eq(accountID) {
			signers := prevBundle.AccountBundles[account.AccountID].Signers
			if len(signers) == 0 {
				return res, fmt.Errorf("no secret keys found for account")
			}
			if len(signers) != 1 {
				return res, fmt.Errorf("expected 1 secret key but found %v", len(signers))
			}
			return signers[0], nil
		}
	}
	_, _, _, parseSecErr := libkb.ParseStellarSecretKey(accountID.String())
	if parseSecErr == nil {
		// Just in case a secret key worked its way in here
		return res, fmt.Errorf("account not found: unexpected secret key")
	}
	return res, fmt.Errorf("account not found: %v", accountID)
}

func OwnAccount(ctx context.Context, g *libkb.GlobalContext, accountID stellar1.AccountID) (own, isPrimary bool, err error) {
	bundle, _, _, err := remote.FetchSecretlessBundle(ctx, g)
	if err != nil {
		return false, false, err
	}
	for _, account := range bundle.Accounts {
		if account.AccountID.Eq(accountID) {
			return true, account.IsPrimary, nil
		}
	}
	return false, false, nil
}

func lookupSenderEntry(ctx context.Context, g *libkb.GlobalContext, accountID stellar1.AccountID) (stellar1.BundleEntryRestricted, stellar1.AccountBundle, error) {
	if accountID == "" {
		bundle, _, _, err := remote.FetchSecretlessBundle(ctx, g)
		if err != nil {
			return stellar1.BundleEntryRestricted{}, stellar1.AccountBundle{}, err
		}
		entry, err := bundle.PrimaryAccount()
		if err != nil {
			return stellar1.BundleEntryRestricted{}, stellar1.AccountBundle{}, err
		}
		accountID = entry.AccountID
	}

	bundle, _, _, err := remote.FetchAccountBundle(ctx, g, accountID)
	if err != nil {
		return stellar1.BundleEntryRestricted{}, stellar1.AccountBundle{}, err
	}

	for _, entry := range bundle.Accounts {
		if entry.AccountID.Eq(accountID) {
			return entry, bundle.AccountBundles[entry.AccountID], nil
		}
	}

	return stellar1.BundleEntryRestricted{}, stellar1.AccountBundle{}, libkb.NotFoundError{Msg: fmt.Sprintf("Sender account not found")}
}

func LookupSender(ctx context.Context, g *libkb.GlobalContext, accountID stellar1.AccountID) (stellar1.BundleEntryRestricted, stellar1.AccountBundle, error) {
	entry, ab, err := lookupSenderEntry(ctx, g, accountID)
	if err != nil {
		return stellar1.BundleEntryRestricted{}, stellar1.AccountBundle{}, err
	}
	if len(ab.Signers) == 0 {
		return stellar1.BundleEntryRestricted{}, stellar1.AccountBundle{}, errors.New("no signer for bundle")
	}
	if len(ab.Signers) > 1 {
		return stellar1.BundleEntryRestricted{}, stellar1.AccountBundle{}, errors.New("only single signer supported")
	}

	return entry, ab, nil
}

// LookupRecipient finds a recipient.
// `to` can be a username, social assertion, account ID, or federation address.
func LookupRecipient(m libkb.MetaContext, to stellarcommon.RecipientInput, isCLI bool) (res stellarcommon.Recipient, err error) {
	defer m.CTraceTimed("Stellar.LookupRecipient", func() error { return err })()
	res = stellarcommon.Recipient{
		Input: to,
	}
	if len(to) == 0 {
		return res, fmt.Errorf("empty recipient parameter")
	}

	storeAddress := func(address string) error {
		_, err := libkb.ParseStellarAccountID(address)
		if err != nil {
			if verr, ok := err.(libkb.VerboseError); ok {
				m.CDebugf(verr.Verbose())
			}
			return err
		}
		accountID, err := stellarnet.NewAddressStr(address)
		if err != nil {
			return err
		}
		res.AccountID = &accountID
		return nil
	}

	// Federation address
	if strings.Contains(string(to), stellarAddress.Separator) {
		name, domain, err := stellarAddress.Split(string(to))
		if err != nil {
			return res, err
		}

		if domain == "keybase.io" {
			// Keybase.io federation address. Fall through to identify
			// path.
			m.CDebugf("Got federation address %q but it's under keybase.io domain!", to)
			m.CDebugf("Instead going to lookup Keybase assertion: %q", name)
			to = stellarcommon.RecipientInput(name)
		} else {
			// Actual federation address that is not under keybase.io
			// domain. Use federation client.
			fedCli := getGlobal(m.G()).federationClient
			nameResponse, err := fedCli.LookupByAddress(string(to))
			if err != nil {
				errStr := err.Error()
				m.CDebugf("federation.LookupByAddress returned error: %s", errStr)
				if strings.Contains(errStr, "lookup federation server failed") {
					return res, fmt.Errorf("Server at url %q does not respond to federation requests", domain)
				} else if strings.Contains(errStr, "get federation failed") {
					return res, fmt.Errorf("Federation server %q did not find record %q", domain, name)
				}
				return res, err
			}
			// We got an address! Fall through to the "Stellar
			// address" path.
			m.CDebugf("federation.LookupByAddress returned: %+v", nameResponse)
			to = stellarcommon.RecipientInput(nameResponse.AccountID)
		}
	}

	// Stellar account ID
	if to[0] == 'G' && len(to) > 16 {
		err := storeAddress(string(to))
		return res, err
	}

	maybeUsername, err := lookupRecipientAssertion(m, string(to), isCLI)
	if err != nil {
		return res, err
	}
	if maybeUsername == "" {
		expr, err := externals.AssertionParse(m.G(), string(to))
		if err != nil {
			m.CDebugf("error parsing assertion: %s", err)
			return res, fmt.Errorf("invalid recipient %q: %s", to, err)
		}

		// valid assertion, but not a user yet
		m.CDebugf("assertion %s (%s) is valid, but not a user yet", to, expr)
		social, err := expr.ToSocialAssertion()
		if err != nil {
			m.CDebugf("not a social assertion: %s (%s)", to, expr)
			if _, ok := expr.(libkb.AssertionKeybase); ok {
				return res, libkb.NotFoundError{Msg: fmt.Sprintf("user not found: %q", to)}
			}
			return res, fmt.Errorf("invalid recipient %q: %s", to, err)
		}
		res.Assertion = &social
		return res, nil
	}

	// load the user to get their wallet
	user, err := libkb.LoadUser(
		libkb.NewLoadUserByNameArg(m.G(), maybeUsername).
			WithNetContext(m.Ctx()).
			WithPublicKeyOptional())
	if err != nil {
		return res, err
	}
	res.User = &stellarcommon.User{
		UV:       user.ToUserVersion(),
		Username: user.GetNormalizedName(),
	}
	accountID := user.StellarAccountID()
	if accountID == nil {
		return res, nil
	}
	err = storeAddress(accountID.String())
	return res, err
}

type DisplayBalance struct {
	Amount   string
	Currency string
}

type SendPaymentArg struct {
	From           stellar1.AccountID // Optional. Defaults to primary account.
	To             stellarcommon.RecipientInput
	Amount         string // Amount of XLM to send.
	DisplayBalance DisplayBalance
	SecretNote     string // Optional.
	PublicMemo     string // Optional.
	ForceRelay     bool
	QuickReturn    bool
}

type SendPaymentResult struct {
	KbTxID stellar1.KeybaseTransactionID
	// Direct: tx ID of the payment tx
	// Relay : tx ID of the funding payment tx
	TxID    stellar1.TransactionID
	Pending bool
	// Implicit team that the relay secret is encrypted for.
	// Present if this was a relay transfer.
	RelayTeamID *keybase1.TeamID
}

// SendPaymentCLI sends XLM from CLI.
func SendPaymentCLI(m libkb.MetaContext, walletState *WalletState, sendArg SendPaymentArg) (res SendPaymentResult, err error) {
	return sendPayment(m, walletState, sendArg, true)
}

// SendPaymentGUI sends XLM from GUI.
func SendPaymentGUI(m libkb.MetaContext, walletState *WalletState, sendArg SendPaymentArg) (res SendPaymentResult, err error) {
	return sendPayment(m, walletState, sendArg, false)
}

// sendPayment sends XLM.
// Recipient:
// Stellar address        : Standard payment
// User with wallet ready : Standard payment
// User without a wallet  : Relay payment
// Unresolved assertion   : Relay payment
func sendPayment(m libkb.MetaContext, walletState *WalletState, sendArg SendPaymentArg, isCLI bool) (res SendPaymentResult, err error) {
	defer m.CTraceTimed("Stellar.SendPayment", func() error { return err })()

	// look up sender account
	senderEntry, senderAccountBundle, err := LookupSender(m.Ctx(), m.G(), sendArg.From)
	if err != nil {
		return res, err
	}
	senderSeed := senderAccountBundle.Signers[0]

	// look up recipient
	recipient, err := LookupRecipient(m, sendArg.To, isCLI)
	if err != nil {
		return res, err
	}

	m.CDebugf("using stellar network passphrase: %q", stellarnet.Network().Passphrase)

	if recipient.AccountID == nil || sendArg.ForceRelay {
		return sendRelayPayment(m, walletState,
			senderSeed, recipient, sendArg.Amount, sendArg.DisplayBalance,
			sendArg.SecretNote, sendArg.PublicMemo, sendArg.QuickReturn)
	}

	ownRecipient, _, err := OwnAccount(m.Ctx(), m.G(),
		stellar1.AccountID(recipient.AccountID.String()))
	if err != nil {
		m.CDebugf("error determining if user own's recipient: %v", err)
		return res, err
	}
	if ownRecipient {
		// When sending to an account that we own, act as though sending to a user as opposed to just an account ID.
		uv, un := m.G().ActiveDevice.GetUsernameAndUserVersionIfValid(m)
		if uv.IsNil() || un.IsNil() {
			m.CDebugf("error finding self: uv:%v un:%v", uv, un)
			return res, fmt.Errorf("error getting logged-in user")
		}
		recipient.User = &stellarcommon.User{
			UV:       uv,
			Username: un,
		}
	}

	senderSeed2, err := stellarnet.NewSeedStr(senderSeed.SecureNoLogString())
	if err != nil {
		return res, err
	}

	post := stellar1.PaymentDirectPost{
		FromDeviceID:    m.G().ActiveDevice.DeviceID(),
		DisplayAmount:   sendArg.DisplayBalance.Amount,
		DisplayCurrency: sendArg.DisplayBalance.Currency,
		QuickReturn:     sendArg.QuickReturn,
	}
	if recipient.User != nil {
		post.To = &recipient.User.UV
	}

	sp := NewSeqnoProvider(m, walletState)

	// check if recipient account exists
	var txID string
	funded, err := isAccountFunded(m.Ctx(), walletState, stellar1.AccountID(recipient.AccountID.String()))
	if err != nil {
		return res, fmt.Errorf("error checking destination account balance: %v", err)
	}
	if !funded {
		// if no balance, create_account operation
		// we could check here to make sure that amount is at least 1XLM
		// but for now, just let stellar-core tell us there was an error
		sig, err := stellarnet.CreateAccountXLMTransaction(senderSeed2, *recipient.AccountID, sendArg.Amount, sendArg.PublicMemo, sp)
		if err != nil {
			return res, err
		}
		post.SignedTransaction = sig.Signed
		txID = sig.TxHash
	} else {
		// if balance, payment operation
		sig, err := stellarnet.PaymentXLMTransaction(senderSeed2, *recipient.AccountID, sendArg.Amount, sendArg.PublicMemo, sp)
		if err != nil {
			return res, err
		}
		post.SignedTransaction = sig.Signed
		txID = sig.TxHash
	}

	if len(sendArg.SecretNote) > 0 {
		noteClear := stellar1.NoteContents{
			Note:      sendArg.SecretNote,
			StellarID: stellar1.TransactionID(txID),
		}
		var recipientUv *keybase1.UserVersion
		if recipient.User != nil {
			recipientUv = &recipient.User.UV
		}
		post.NoteB64, err = NoteEncryptB64(m.Ctx(), m.G(), noteClear, recipientUv)
		if err != nil {
			return res, fmt.Errorf("error encrypting note: %v", err)
		}
	}

	// submit the transaction
	rres, err := walletState.SubmitPayment(m.Ctx(), post)
	if err != nil {
		return res, err
	}

	if senderEntry.IsPrimary {
		sendChat := func(mctx libkb.MetaContext) {
			if err := chatSendPaymentMessage(mctx, recipient, rres.StellarID); err != nil {
				// if the chat message fails to send, just log the error
				mctx.CDebugf("failed to send chat SendPayment message: %s", err)
			}
		}
		if sendArg.QuickReturn {
			go sendChat(m.WithCtx(context.Background()))
		} else {
			sendChat(m)
		}
	} else {
		m.CDebugf("not sending chat message: sending from non-primary account")
	}

	return SendPaymentResult{
		KbTxID:  rres.KeybaseID,
		TxID:    rres.StellarID,
		Pending: rres.Pending,
	}, nil
}

// SendMiniChatPayments sends multiple payments from one sender to multiple
// different recipients as fast as it can.  These come from chat messages
// like "+1XLM@alice +2XLM@charlie".
func SendMiniChatPayments(m libkb.MetaContext, walletState *WalletState, payments []libkb.MiniChatPayment) ([]libkb.MiniChatPaymentResult, error) {
	// look up sender account
	_, senderAccountBundle, err := LookupSender(m.Ctx(), m.G(), "" /* empty account id returns primary */)
	if err != nil {
		return nil, err
	}
	senderSeed, err := stellarnet.NewSeedStr(senderAccountBundle.Signers[0].SecureNoLogString())
	if err != nil {
		return nil, err
	}

	prepared := make(chan *miniPrepared)

	sp := NewSeqnoProvider(m, walletState)

	for _, payment := range payments {
		go func(p libkb.MiniChatPayment) {
			prepared <- prepareMiniChatPayment(m, walletState, sp, senderSeed, p)
		}(payment)
	}

	// prepared chan could be out of order, so sort by seqno
	preparedList := make([]*miniPrepared, len(payments))
	for i := 0; i < len(payments); i++ {
		preparedList[i] = <-prepared
	}
	sort.Slice(preparedList, func(a, b int) bool { return preparedList[a].Seqno < preparedList[b].Seqno })

	resultList := make([]libkb.MiniChatPaymentResult, len(payments))

	// need to submit tx one at a time, in order
	for i := 0; i < len(preparedList); i++ {
		mcpResult := libkb.MiniChatPaymentResult{Username: preparedList[i].Username}
		if preparedList[i].Error != nil {
			mcpResult.Error = preparedList[i].Error
		} else {
			// submit the transaction
			m.CDebugf("submitting payment seqno %d", preparedList[i].Seqno)
			submitRes, err := walletState.SubmitPayment(m.Ctx(), preparedList[i].Post)
			if err != nil {
				mcpResult.Error = err
			} else {
				mcpResult.PaymentID = stellar1.NewPaymentID(submitRes.StellarID)
			}
		}
		resultList[i] = mcpResult
	}

	return resultList, nil
}

type miniPrepared struct {
	Username libkb.NormalizedUsername
	Post     stellar1.PaymentDirectPost
	Seqno    uint64
	Error    error
}

func prepareMiniChatPayment(m libkb.MetaContext, remoter remote.Remoter, sp build.SequenceProvider, senderSeed stellarnet.SeedStr, payment libkb.MiniChatPayment) *miniPrepared {
	result := &miniPrepared{Username: payment.Username}

	recipient, err := LookupRecipient(m, stellarcommon.RecipientInput(payment.Username.String()), false)
	if err != nil {
		result.Error = err
		return result
	}

	if recipient.AccountID == nil {
		result.Error = errors.New("user has no stellar account")
		return result
	}

	funded, err := isAccountFunded(m.Ctx(), remoter, stellar1.AccountID(recipient.AccountID.String()))
	if err != nil {
		result.Error = err
		return result
	}

	result.Post = stellar1.PaymentDirectPost{
		FromDeviceID: m.G().ActiveDevice.DeviceID(),
		To:           &recipient.User.UV,
		QuickReturn:  true,
	}

	xlmAmount := payment.Amount
	if payment.Currency != "" && payment.Currency != "XLM" {
		result.Post.DisplayAmount = payment.Amount
		result.Post.DisplayCurrency = payment.Currency
		exchangeRate, err := remoter.ExchangeRate(m.Ctx(), payment.Currency)
		if err != nil {
			result.Error = err
			return result
		}

		xlmAmount, err = stellarnet.ConvertOutsideToXLM(payment.Amount, exchangeRate.Rate)
		if err != nil {
			result.Error = err
			return result
		}
	}

	var signResult stellarnet.SignResult
	if funded {
		signResult, err = stellarnet.PaymentXLMTransaction(senderSeed, *recipient.AccountID, xlmAmount, "", sp)
	} else {
		signResult, err = stellarnet.CreateAccountXLMTransaction(senderSeed, *recipient.AccountID, xlmAmount, "", sp)
	}
	if err != nil {
		result.Error = err
		return result
	}
	result.Post.SignedTransaction = signResult.Signed
	result.Seqno = signResult.Seqno
	return result

}

// sendRelayPayment sends XLM through a relay account.
// The balance of the relay account can be claimed by either party.
func sendRelayPayment(m libkb.MetaContext, walletState *WalletState,
	from stellar1.SecretKey, recipient stellarcommon.Recipient, amount string, displayBalance DisplayBalance,
	secretNote string, publicMemo string, quickReturn bool) (res SendPaymentResult, err error) {
	defer m.CTraceTimed("Stellar.sendRelayPayment", func() error { return err })()
	appKey, teamID, err := relays.GetKey(m.Ctx(), m.G(), recipient)
	if err != nil {
		return res, err
	}
	sp := NewSeqnoProvider(m, walletState)
	relay, err := relays.Create(relays.Input{
		From:          from,
		AmountXLM:     amount,
		Note:          secretNote,
		PublicMemo:    publicMemo,
		EncryptFor:    appKey,
		SeqnoProvider: sp,
	})
	if err != nil {
		return res, err
	}
	post := stellar1.PaymentRelayPost{
		FromDeviceID:      m.G().ActiveDevice.DeviceID(),
		ToAssertion:       string(recipient.Input),
		RelayAccount:      relay.RelayAccountID,
		TeamID:            teamID,
		BoxB64:            relay.EncryptedB64,
		SignedTransaction: relay.FundTx.Signed,
		DisplayAmount:     displayBalance.Amount,
		DisplayCurrency:   displayBalance.Currency,
		QuickReturn:       quickReturn,
	}
	if recipient.User != nil {
		post.To = &recipient.User.UV
	}
	rres, err := walletState.SubmitRelayPayment(m.Ctx(), post)
	if err != nil {
		return res, err
	}

	if err := chatSendPaymentMessage(m, recipient, rres.StellarID); err != nil {
		// if the chat message fails to send, just log the error
		m.CDebugf("failed to send chat SendPayment message: %s", err)
	}

	return SendPaymentResult{
		KbTxID:      rres.KeybaseID,
		TxID:        rres.StellarID,
		Pending:     rres.Pending,
		RelayTeamID: &teamID,
	}, nil
}

// Claim claims a waiting relay.
// If `dir` is nil the direction is inferred.
func Claim(ctx context.Context, g *libkb.GlobalContext, walletState *WalletState,
	txID string, into stellar1.AccountID, dir *stellar1.RelayDirection,
	autoClaimToken *string) (res stellar1.RelayClaimResult, err error) {
	defer g.CTraceTimed(ctx, "Stellar.Claim", func() error { return err })()
	g.Log.CDebugf(ctx, "Stellar.Claim(txID:%v, into:%v, dir:%v, autoClaimToken:%v)", txID, into, dir, autoClaimToken)
	details, err := walletState.PaymentDetails(ctx, txID)
	if err != nil {
		return res, err
	}
	p := details.Summary
	typ, err := p.Typ()
	if err != nil {
		return res, fmt.Errorf("error getting payment details: %v", err)
	}
	switch typ {
	case stellar1.PaymentSummaryType_STELLAR:
		return res, fmt.Errorf("Payment cannot be claimed. It was found on the Stellar network but not in Keybase.")
	case stellar1.PaymentSummaryType_DIRECT:
		p := p.Direct()
		switch p.TxStatus {
		case stellar1.TransactionStatus_SUCCESS:
			return res, fmt.Errorf("Payment cannot be claimed. The direct transfer already happened.")
		case stellar1.TransactionStatus_PENDING:
			return res, fmt.Errorf("Payment cannot be claimed. It is currently pending.")
		default:
			return res, fmt.Errorf("Payment cannot be claimed. The payment failed anyway.")
		}
	case stellar1.PaymentSummaryType_RELAY:
		return claimPaymentWithDetail(ctx, g, walletState, p.Relay(), into, dir)
	default:
		return res, fmt.Errorf("unrecognized payment type: %v", typ)
	}
}

// If `dir` is nil the direction is inferred.
func claimPaymentWithDetail(ctx context.Context, g *libkb.GlobalContext, walletState *WalletState,
	p stellar1.PaymentSummaryRelay, into stellar1.AccountID, dir *stellar1.RelayDirection) (res stellar1.RelayClaimResult, err error) {
	if p.Claim != nil && p.Claim.TxStatus == stellar1.TransactionStatus_SUCCESS {
		recipient, _, err := g.GetUPAKLoader().Load(libkb.NewLoadUserByUIDArg(ctx, g, p.Claim.To.Uid))
		if err != nil || recipient == nil {
			return res, fmt.Errorf("Payment already claimed")
		}
		return res, fmt.Errorf("Payment already claimed by %v", recipient.GetName())
	}
	rsec, err := relays.DecryptB64(ctx, g, p.TeamID, p.BoxB64)
	if err != nil {
		return res, fmt.Errorf("error opening secret to claim: %v", err)
	}
	skey, _, _, err := libkb.ParseStellarSecretKey(rsec.Sk.SecureNoLogString())
	if err != nil {
		return res, fmt.Errorf("error using shared secret key: %v", err)
	}
	destinationFunded, err := isAccountFunded(ctx, walletState, into)
	if err != nil {
		return res, err
	}
	useDir := stellar1.RelayDirection_CLAIM
	if dir == nil {
		// Infer direction
		if p.From.Uid.Equal(g.ActiveDevice.UID()) {
			useDir = stellar1.RelayDirection_YANK
		}
	} else {
		// Direction from caller
		useDir = *dir
	}
	sp := NewSeqnoProvider(libkb.NewMetaContext(ctx, g), walletState)
	// Throw a random ID into the transaction memo so that we get a new txID each time.
	// This makes it easy to resubmit without hitting a txID collision on the server.
	memoID, err := randMemoID()
	if err != nil {
		return res, err
	}
	g.Log.CDebugf(ctx, "using randomized memo ID: %v", memoID)
	sig, err := stellarnet.RelocateTransaction(stellarnet.SeedStr(skey.SecureNoLogString()),
		stellarnet.AddressStr(into.String()), destinationFunded, &memoID, sp)
	if err != nil {
		return res, fmt.Errorf("error building claim transaction: %v", err)
	}
	return walletState.SubmitRelayClaim(ctx, stellar1.RelayClaimPost{
		KeybaseID:         p.KbTxID,
		Dir:               useDir,
		SignedTransaction: sig.Signed,
	})
}

func randMemoID() (uint64, error) {
	buf, err := libkb.RandBytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf), nil
}

func isAccountFunded(ctx context.Context, remoter remote.Remoter, accountID stellar1.AccountID) (funded bool, err error) {
	balances, err := remoter.Balances(ctx, accountID)
	if err != nil {
		return false, err
	}
	for _, b := range balances {
		if b.Asset.IsNativeXLM() {
			a, err := stellarnet.ParseStellarAmount(b.Amount)
			if err != nil {
				return false, err
			}
			if a > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

func GetOwnPrimaryAccountID(ctx context.Context, g *libkb.GlobalContext) (res stellar1.AccountID, err error) {
	activeBundle, _, _, err := remote.FetchSecretlessBundle(ctx, g)
	if err != nil {
		return res, err
	}
	primary, err := activeBundle.PrimaryAccount()
	if err != nil {
		return res, err
	}
	return primary.AccountID, nil
}

func RecentPaymentsCLILocal(ctx context.Context, g *libkb.GlobalContext, remoter remote.Remoter, accountID stellar1.AccountID) (res []stellar1.PaymentOrErrorCLILocal, err error) {
	defer g.CTraceTimed(ctx, "Stellar.RecentPaymentsCLILocal", func() error { return err })()
	page, err := remoter.RecentPayments(ctx, accountID, nil, 0, false)
	if err != nil {
		return nil, err
	}
	for _, p := range page.Payments {
		lp, err := localizePayment(ctx, g, p)
		if err == nil {
			res = append(res, stellar1.PaymentOrErrorCLILocal{
				Payment: &lp,
			})
		} else {
			errStr := err.Error()
			res = append(res, stellar1.PaymentOrErrorCLILocal{
				Err: &errStr,
			})
		}
	}
	return res, nil
}

func PaymentDetailCLILocal(ctx context.Context, g *libkb.GlobalContext, remoter remote.Remoter, txID string) (res stellar1.PaymentCLILocal, err error) {
	defer g.CTraceTimed(ctx, "Stellar.PaymentDetailCLILocal", func() error { return err })()
	payment, err := remoter.PaymentDetails(ctx, txID)
	if err != nil {
		return res, err
	}
	return localizePayment(ctx, g, payment.Summary)
}

func localizePayment(ctx context.Context, g *libkb.GlobalContext, p stellar1.PaymentSummary) (res stellar1.PaymentCLILocal, err error) {
	typ, err := p.Typ()
	if err != nil {
		return res, fmt.Errorf("malformed payment summary: %v", err)
	}
	username := func(uid keybase1.UID) (username *string, err error) {
		uname, err := g.GetUPAKLoader().LookupUsername(ctx, uid)
		if err != nil {
			return nil, err
		}
		tmp := uname.String()
		return &tmp, nil
	}
	switch typ {
	case stellar1.PaymentSummaryType_STELLAR:
		p := p.Stellar()
		return stellar1.PaymentCLILocal{
			TxID:        p.TxID,
			Time:        p.Ctime,
			Status:      "Completed",
			Amount:      p.Amount,
			Asset:       p.Asset,
			FromStellar: p.From,
			ToStellar:   &p.To,
			Unread:      p.Unread,
		}, nil
	case stellar1.PaymentSummaryType_DIRECT:
		p := p.Direct()
		res = stellar1.PaymentCLILocal{
			TxID:            p.TxID,
			Time:            p.Ctime,
			Amount:          p.Amount,
			Asset:           p.Asset,
			DisplayAmount:   p.DisplayAmount,
			DisplayCurrency: p.DisplayCurrency,
			FromStellar:     p.FromStellar,
			ToStellar:       &p.ToStellar,
		}
		res.Status, res.StatusDetail = p.TxStatus.Details(p.TxErrMsg)
		res.FromUsername, err = username(p.From.Uid)
		if err != nil {
			return res, err
		}
		if p.To != nil {
			res.ToUsername, err = username(p.To.Uid)
			if err != nil {
				return res, err
			}
		}
		if len(p.NoteB64) > 0 {
			note, err := NoteDecryptB64(ctx, g, p.NoteB64)
			if err != nil {
				res.NoteErr = fmt.Sprintf("failed to decrypt payment note: %v", err)
			} else {
				if note.StellarID != p.TxID {
					res.NoteErr = "discarded note for wrong transaction ID"
				} else {
					res.Note = note.Note
				}
			}
			if len(res.NoteErr) > 0 {
				g.Log.CWarningf(ctx, res.NoteErr)
			}
		}
		return res, nil
	case stellar1.PaymentSummaryType_RELAY:
		p := p.Relay()
		res = stellar1.PaymentCLILocal{
			TxID:            p.TxID,
			Time:            p.Ctime,
			Amount:          p.Amount,
			Asset:           stellar1.AssetNative(),
			DisplayAmount:   p.DisplayAmount,
			DisplayCurrency: p.DisplayCurrency,
			FromStellar:     p.FromStellar,
		}
		if p.TxStatus != stellar1.TransactionStatus_SUCCESS {
			// If the funding tx is not complete
			res.Status, res.StatusDetail = p.TxStatus.Details(p.TxErrMsg)
		} else {
			res.Status = "Claimable"
			res.StatusDetail = "Waiting for the recipient to open the app to claim, or the sender to cancel."
		}
		res.FromUsername, err = username(p.From.Uid)
		if err != nil {
			return res, err
		}
		if p.To != nil {
			res.ToUsername, err = username(p.To.Uid)
			if err != nil {
				return res, err
			}
		}
		if p.ToAssertion != "" {
			res.ToAssertion = &p.ToAssertion
		}
		// Override status with claim status
		if p.Claim != nil {
			if p.Claim.TxStatus == stellar1.TransactionStatus_SUCCESS {
				// If the claim succeeded, the relay payment is done.
				res.Status = "Completed"
				res.ToStellar = &p.Claim.ToStellar
				res.ToUsername, err = username(p.Claim.To.Uid)
				if err != nil {
					return res, err
				}
			} else {
				claimantUsername, err := username(p.Claim.To.Uid)
				if err != nil {
					return res, err
				}
				res.Status, res.StatusDetail = p.Claim.TxStatus.Details(p.Claim.TxErrMsg)
				res.Status = fmt.Sprintf("Funded. Claim by %v is: %v", *claimantUsername, res.Status)
			}
		}
		relaySecrets, err := relays.DecryptB64(ctx, g, p.TeamID, p.BoxB64)
		if err == nil {
			res.Note = relaySecrets.Note
		} else {
			res.NoteErr = fmt.Sprintf("error decrypting note box: %v", err)
		}
		return res, nil
	default:
		return res, fmt.Errorf("unrecognized payment summary type: %v", typ)
	}
}

// When isCLI : Identifies the recipient checking track breaks and all.
// When not isCLI: Does a verified lookup of the assertion.
// Returns an error if a resolution was found but failed.
// Returns ("", nil) if no resolution was found.
func lookupRecipientAssertion(m libkb.MetaContext, assertion string, isCLI bool) (maybeUsername string, err error) {
	defer m.CTraceTimed(fmt.Sprintf("Stellar.lookupRecipientAssertion(isCLI:%v, %v)", isCLI, assertion), func() error { return err })()
	reason := fmt.Sprintf("Find transaction recipient for %s", assertion)
	// GUI is a verified lookup modeled after func ResolveAndCheck.
	arg := keybase1.Identify2Arg{
		UserAssertion:         assertion,
		CanSuppressUI:         true,
		ActLoggedOut:          true,
		NoErrorOnTrackFailure: true,
		Reason:                keybase1.IdentifyReason{Reason: reason},
		IdentifyBehavior:      keybase1.TLFIdentifyBehavior_RESOLVE_AND_CHECK,
	}
	if isCLI {
		// CLI is a real identify
		arg = keybase1.Identify2Arg{
			UserAssertion:    assertion,
			UseDelegateUI:    true,
			Reason:           keybase1.IdentifyReason{Reason: reason},
			IdentifyBehavior: keybase1.TLFIdentifyBehavior_CLI,
		}
	}

	eng := engine.NewResolveThenIdentify2(m.G(), &arg)
	err = engine.RunEngine2(m, eng)
	if err != nil {
		// These errors mean no resolution was found.
		if _, ok := err.(libkb.NotFoundError); ok {
			m.CDebugf("identifyRecipient: not found %s: %s", assertion, err)
			return "", nil
		}
		if libkb.IsResolutionNotFoundError(err) {
			m.CDebugf("identifyRecipient: resolution not found error %s: %s", assertion, err)
			return "", nil
		}
		return "", err
	}

	idRes, err := eng.Result(m)
	if err != nil {
		return "", err
	}
	if idRes == nil {
		return "", fmt.Errorf("missing identify result")
	}
	m.CDebugf("lookupRecipientAssertion: uv: %v", idRes.Upk.Current.ToUserVersion())
	username := idRes.Upk.GetName()
	if username == "" {
		return "", fmt.Errorf("empty identify result username")
	}
	if isCLI && idRes.TrackBreaks != nil {
		m.CDebugf("lookupRecipientAssertion: TrackBreaks = %+v", idRes.TrackBreaks)
		return "", libkb.TrackingBrokeError{}
	}
	return username, nil
}

type FmtRounding bool

const FmtRound = false
const FmtTruncate = true

func FormatCurrency(ctx context.Context, g *libkb.GlobalContext,
	amount string, code stellar1.OutsideCurrencyCode, rounding FmtRounding) (string, error) {
	conf, err := g.GetStellar().GetServerDefinitions(ctx)
	if err != nil {
		return "", err
	}
	currency, ok := conf.Currencies[code]
	if !ok {
		return "", fmt.Errorf("FormatCurrency error: cannot find curency code %q", code)
	}

	amountFmt, err := FormatAmount(amount, true, rounding)
	if err != nil {
		return "", err
	}

	if currency.Symbol.Postfix {
		return fmt.Sprintf("%s %s", amountFmt, currency.Symbol.Symbol), nil
	}

	return fmt.Sprintf("%s%s", currency.Symbol.Symbol, amountFmt), nil
}

// FormatCurrencyWithCodeSuffix will return a fiat currency amount formatted with
// its currency code suffix at the end, like "$123.12 CLP"
func FormatCurrencyWithCodeSuffix(ctx context.Context, g *libkb.GlobalContext,
	amount string, code stellar1.OutsideCurrencyCode, rounding FmtRounding) (string, error) {
	pre, err := FormatCurrency(ctx, g, amount, code, rounding)
	if err != nil {
		return "", err
	}

	// some currencies have the same symbol as code (CHF)
	conf, err := g.GetStellar().GetServerDefinitions(ctx)
	if err != nil {
		return "", err
	}
	currency, ok := conf.Currencies[code]
	if !ok {
		return "", fmt.Errorf("FormatCurrency error: cannot find curency code %q", code)
	}
	if currency.Symbol.Postfix && currency.Symbol.Symbol == code.String() {
		return pre, nil
	}

	return fmt.Sprintf("%s %s", pre, code), nil
}

func FormatCurrencyLabel(ctx context.Context, g *libkb.GlobalContext, code stellar1.OutsideCurrencyCode) (string, error) {
	conf, err := g.GetStellar().GetServerDefinitions(ctx)
	if err != nil {
		return "", err
	}
	currency, ok := conf.Currencies[code]
	if !ok {
		return "", fmt.Errorf("FormatCurrencyLabel error: cannot find curency code %q", code)
	}
	return fmt.Sprintf("%s (%s)", code, currency.Symbol.Symbol), nil
}

// Return an error if asset is completely outside of what we understand, like
// asset unknown types or unexpected length.
func assertAssetIsSane(asset stellar1.Asset) error {
	switch asset.Type {
	case "credit_alphanum4", "credit_alphanum12":
	case "alphanum4", "alphanum12": // These prefixes that are missing "credit_" shouldn't show up, but just to be on the safe side.
	default:
		return fmt.Errorf("unrecognized asset type: %v", asset.Type)
	}
	// Sanity check asset code very loosely. We know tighter bounds but there's no need to fail here.
	if len(asset.Code) <= 0 || len(asset.Code) >= 20 {
		return fmt.Errorf("invalid asset code: %v", asset.Code)
	}
	return nil
}

// Example: "157.5000000 XLM"
// Example: "12.9000000 USD"
//   (where USD is a non-native asset issued by someone).
// User interfaces should be careful to never give user just amount + asset
// code, but annotate when it's a non-native asset and make Issuer ID and
// Verified Domain visible.
// If you are coming from CLI, FormatAmountDescriptionAssetEx might be a better
// choice which is more verbose about non-native assets.
func FormatAmountDescriptionAsset(amount string, asset stellar1.Asset) (string, error) {
	if asset.IsNativeXLM() {
		return FormatAmountDescriptionXLM(amount)
	}
	if err := assertAssetIsSane(asset); err != nil {
		return "", err
	}
	// Sanity check asset issuer.
	if _, err := libkb.ParseStellarAccountID(asset.Issuer); err != nil {
		return "", fmt.Errorf("asset issuer is not account ID: %v", asset.Issuer)
	}
	return FormatAmountWithSuffix(amount, false /* precisionTwo */, false /* simplify */, asset.Code)
}

// FormatAmountDescriptionAssetEx is a more verbose version of FormatAmountDescriptionAsset.
// In case of non-native asset, it includes issuer domain (or "Unknown") and issuer ID.
// Example: "157.5000000 XLM"
// Example: "1,000.15 CATS/catmoney.example.com (GDWVJEG7CMYKRYGB2MWSRZNSPCWIGGA4FRNFTQBIR6RAEPNEGGEH4XYZ)"
// Example: "1,000.15 BTC/Unknown (GBPEHURSE52GCBRPDWNV2VL3HRLCI42367OGRPBOO3AW6VAYEW5EO5PM)"
func FormatAmountDescriptionAssetEx(amount string, asset stellar1.Asset) (string, error) {
	if asset.IsNativeXLM() {
		return FormatAmountDescriptionXLM(amount)
	}
	if err := assertAssetIsSane(asset); err != nil {
		return "", err
	}
	// Sanity check asset issuer.
	issuerAccountID, err := libkb.ParseStellarAccountID(asset.Issuer)
	if err != nil {
		return "", fmt.Errorf("asset issuer is not account ID: %v", asset.Issuer)
	}
	amountFormatted, err := FormatAmount(amount, false /* precisionTwo */, FmtRound)
	if err != nil {
		return "", err
	}
	var issuerDesc string
	if asset.VerifiedDomain != "" {
		issuerDesc = asset.VerifiedDomain
	} else {
		issuerDesc = "Unknown"
	}
	return fmt.Sprintf("%s %s/%s (%s)", amountFormatted, asset.Code, issuerDesc, issuerAccountID.String()), nil
}

// FormatAssetIssuerString returns "Unknown issuer" if asset does not have a
// verified domain, or returns asset verified domain if it does (e.g.
// "example.com").
func FormatAssetIssuerString(asset stellar1.Asset) string {
	if asset.VerifiedDomain != "" {
		return asset.VerifiedDomain
	}
	return "Unknown issuer"
}

// Example: "157.5000000 XLM"
func FormatAmountDescriptionXLM(amount string) (string, error) {
	// Do not simplify XLM amounts, all zeroes are important because
	// that's the exact number of digits that Stellar protocol
	// supports.
	return FormatAmountWithSuffix(amount, false /* precisionTwo */, false /* simplify */, "XLM")
}

func FormatAmountWithSuffix(amount string, precisionTwo bool, simplify bool, suffix string) (string, error) {
	formatted, err := FormatAmount(amount, precisionTwo, FmtRound)
	if err != nil {
		return "", err
	}
	if simplify {
		formatted = libkb.StellarSimplifyAmount(formatted)
	}
	return fmt.Sprintf("%s %s", formatted, suffix), nil
}

func FormatAmount(amount string, precisionTwo bool, rounding FmtRounding) (string, error) {
	if amount == "" {
		return "", errors.New("empty amount")
	}
	x, err := stellarnet.ParseAmount(amount)
	if err != nil {
		return "", fmt.Errorf("unable to parse amount %s: %v", amount, err)
	}
	precision := 7
	if precisionTwo {
		precision = 2
	}
	var s string
	if rounding == FmtRound {
		s = x.FloatString(precision)
	} else {
		s = x.FloatString(precision + 1)
		s = s[:len(s)-1]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("unable to parse amount %s", amount)
	}
	var hasComma bool
	head := parts[0]
	if len(head) > 0 {
		sinceComma := 0
		var b bytes.Buffer
		for i := len(head) - 1; i >= 0; i-- {
			if sinceComma == 3 && head[i] != '-' {
				b.WriteByte(',')
				sinceComma = 0
				hasComma = true
			}
			b.WriteByte(head[i])
			sinceComma++
		}
		parts[0] = reverse(b.String())
	}
	if parts[1] == "0000000" {
		// Remove decimal part if it's all zeroes in 7-digit precision.
		if hasComma {
			// With the exception of big numbers where we inserted
			// thousands separator - leave fractional part with two
			// digits so we can have decimal point, but not all the
			// distracting 7 zeroes.
			parts[1] = "00"
		} else {
			parts = parts[:1]
		}
	}

	return strings.Join(parts, "."), nil
}

func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < len(r)/2; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// ChangeAccountName changes the name of an account.
// Make sure to keep this in sync with ValidateAccountNameLocal.
// An empty name is not allowed.
// Renaming an account to an already used name is blocked.
// Maximum length of AccountNameMaxRunes runes.
func ChangeAccountName(m libkb.MetaContext, accountID stellar1.AccountID, newName string) (err error) {
	if newName == "" {
		return fmt.Errorf("name required")
	}
	runes := utf8.RuneCountInString(newName)
	if runes > AccountNameMaxRunes {
		return fmt.Errorf("account name can be %v characters at the longest but was %v", AccountNameMaxRunes, runes)
	}
	bundle, version, _, err := remote.FetchSecretlessBundle(m.Ctx(), m.G())
	if err != nil {
		return err
	}
	if version == stellar1.BundleVersion_V1 {
		bundle, _, _, err = remote.FetchWholeBundle(m.Ctx(), m.G())
		if err != nil {
			return err
		}
	}
	var found bool
	for i, acc := range bundle.Accounts {
		if acc.AccountID.Eq(accountID) {
			// Change Name in place to modify Account struct.
			bundle.Accounts[i].Name = newName
			found = true
		} else if acc.Name == newName {
			return fmt.Errorf("you already have an account with that name")
		}
	}
	if !found {
		return fmt.Errorf("account not found: %v", accountID)
	}
	nextBundle := acctbundle.AdvanceBundle(*bundle)
	return remote.Post(m.Ctx(), m.G(), nextBundle, version)
}

func SetAccountAsPrimary(m libkb.MetaContext, accountID stellar1.AccountID) (err error) {
	if accountID.IsNil() {
		return errors.New("passed empty AccountID")
	}
	bundle, version, _, err := remote.FetchAccountBundle(m.Ctx(), m.G(), accountID)
	if err != nil {
		return err
	}
	var foundAccID, foundPrimary bool
	for i, acc := range bundle.Accounts {
		if acc.AccountID.Eq(accountID) {
			if acc.IsPrimary {
				// Nothing to do.
				return nil
			}
			bundle.Accounts[i].IsPrimary = true
			foundAccID = true
		} else if acc.IsPrimary {
			bundle.Accounts[i].IsPrimary = false
			foundPrimary = true
		}

		if foundAccID && foundPrimary {
			break
		}
	}
	if !foundAccID {
		return fmt.Errorf("account not found: %v", accountID)
	}
	nextBundle := acctbundle.AdvanceAccounts(*bundle, []stellar1.AccountID{accountID})
	var v2Link bool
	v2Link = !(version == stellar1.BundleVersion_V1)
	return remote.PostWithChainlink(m.Ctx(), m.G(), nextBundle, v2Link)
}

func DeleteAccount(m libkb.MetaContext, accountID stellar1.AccountID) error {
	if accountID.IsNil() {
		return errors.New("passed empty AccountID")
	}
	prevBundle, version, _, err := remote.FetchAccountBundle(m.Ctx(), m.G(), accountID)
	if err != nil {
		return err
	}

	nextBundle := acctbundle.AdvanceBundle(*prevBundle)
	var found bool
	for i, acc := range nextBundle.Accounts {
		if acc.AccountID.Eq(accountID) {
			if acc.IsPrimary {
				return fmt.Errorf("cannot delete primary account %v", accountID)
			}

			nextBundle.Accounts = append(nextBundle.Accounts[:i], nextBundle.Accounts[i+1:]...)
			delete(nextBundle.AccountBundles, accountID)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("account not found: %v", accountID)
	}
	return remote.Post(m.Ctx(), m.G(), nextBundle, version)
}

const DefaultCurrencySetting = "USD"

// GetAccountDisplayCurrency gets currency setting from the server, and it
// returned currency is empty (NULL in database), then default "USD" is used.
// When creating a wallet, client always sets default currency setting. Also
// when a new account in existing wallet is created, it will inherit currency
// setting from primary account (this happens serverside). Empty currency
// settings should only happen in very old accounts or when wallet generation
// was interrupted in precise moment.
func GetAccountDisplayCurrency(mctx libkb.MetaContext, accountID stellar1.AccountID) (res string, err error) {
	codeStr, err := remote.GetAccountDisplayCurrency(mctx.Ctx(), mctx.G(), accountID)
	if err != nil {
		return res, err
	}
	if codeStr == "" {
		codeStr = DefaultCurrencySetting
		mctx.CDebugf("Using default display currency %s for account %s", codeStr, accountID)
	}
	return codeStr, nil
}

func GetCurrencySetting(mctx libkb.MetaContext, remoter remote.Remoter, accountID stellar1.AccountID) (res stellar1.CurrencyLocal, err error) {
	codeStr, err := GetAccountDisplayCurrency(mctx, accountID)
	if err != nil {
		return res, err
	}
	conf, err := mctx.G().GetStellar().GetServerDefinitions(mctx.Ctx())
	if err != nil {
		return res, err
	}
	currency, ok := conf.GetCurrencyLocal(stellar1.OutsideCurrencyCode(codeStr))
	if !ok {
		return res, fmt.Errorf("Got unrecognized currency code %q", codeStr)
	}
	return currency, nil
}

func accountIDFromSecretKey(skey stellar1.SecretKey) (stellar1.AccountID, error) {
	_, res, _, err := libkb.ParseStellarSecretKey(skey.SecureNoLogString())
	return res, err
}

func CreateNewAccount(m libkb.MetaContext, accountName string) (ret stellar1.AccountID, err error) {
	prevBundle, version, _, err := remote.FetchSecretlessBundle(m.Ctx(), m.G())
	if err != nil {
		return ret, err
	}
	if version == stellar1.BundleVersion_V1 {
		prevBundle, _, _, err = remote.FetchWholeBundle(m.Ctx(), m.G())
		if err != nil {
			return ret, err
		}
	}
	nextBundle := acctbundle.AdvanceBundle(*prevBundle)
	ret, err = acctbundle.CreateNewAccount(&nextBundle, accountName, false /* makePrimary */)
	if err != nil {
		return ret, err
	}
	return ret, remote.Post(m.Ctx(), m.G(), nextBundle, version)
}

func chatSendPaymentMessage(m libkb.MetaContext, recipient stellarcommon.Recipient, txID stellar1.TransactionID) error {
	var chatRecipient string
	if recipient.User != nil {
		chatRecipient = recipient.User.Username.String()
	} else if recipient.Assertion != nil {
		chatRecipient = recipient.Assertion.String()
	} else {
		m.CDebugf("Not sending chat message: recipient is not a user or an assertion")
		return nil
	}

	m.G().StartStandaloneChat()
	if m.G().ChatHelper == nil {
		return errors.New("cannot send SendPayment message:  chat helper is nil")
	}

	name := strings.Join([]string{m.CurrentUsername().String(), chatRecipient}, ",")

	msg := chat1.MessageSendPayment{
		PaymentID: stellar1.NewPaymentID(txID),
	}

	body := chat1.NewMessageBodyWithSendpayment(msg)

	// identify already performed, so skip here
	return m.G().ChatHelper.SendMsgByNameNonblock(m.Ctx(), name, nil, chat1.ConversationMembersType_IMPTEAMNATIVE, keybase1.TLFIdentifyBehavior_CHAT_SKIP, body, chat1.MessageType_SENDPAYMENT)
}

type MakeRequestArg struct {
	To       stellarcommon.RecipientInput
	Amount   string
	Asset    *stellar1.Asset
	Currency *stellar1.OutsideCurrencyCode
	Note     string
}

func MakeRequestGUI(m libkb.MetaContext, remoter remote.Remoter, arg MakeRequestArg) (ret stellar1.KeybaseRequestID, err error) {
	return makeRequest(m, remoter, arg, false /* isCLI */)
}

func MakeRequestCLI(m libkb.MetaContext, remoter remote.Remoter, arg MakeRequestArg) (ret stellar1.KeybaseRequestID, err error) {
	return makeRequest(m, remoter, arg, true /* isCLI */)
}

func makeRequest(m libkb.MetaContext, remoter remote.Remoter, arg MakeRequestArg, isCLI bool) (ret stellar1.KeybaseRequestID, err error) {
	defer m.CTraceTimed("Stellar.MakeRequest", func() error { return err })()

	if arg.Asset == nil && arg.Currency == nil {
		return ret, fmt.Errorf("expected either Asset or Currency, got none")
	} else if arg.Asset != nil && arg.Currency != nil {
		return ret, fmt.Errorf("expected either Asset or Currency, got both")
	}

	if arg.Asset != nil && !arg.Asset.IsNativeXLM() {
		return ret, fmt.Errorf("requesting non-XLM assets is not supported")
	}

	if arg.Asset != nil {
		a, err := stellarnet.ParseStellarAmount(arg.Amount)
		if err != nil {
			return ret, err
		}
		if a <= 0 {
			return ret, fmt.Errorf("must request positive amount of XLM")
		}
	}

	if arg.Currency != nil {
		conf, err := m.G().GetStellar().GetServerDefinitions(m.Ctx())
		if err != nil {
			return ret, err
		}
		_, ok := conf.GetCurrencyLocal(*arg.Currency)
		if !ok {
			return ret, fmt.Errorf("unrecognized currency code %q", arg.Currency)
		}
	}

	// Make sure chat is functional. Chat message is the only way for
	// the recipient to learn about the request, so it's essential
	// that we are able to send REQUESTPAYMENT chat message.
	m.G().StartStandaloneChat()
	if m.G().ChatHelper == nil {
		return ret, errors.New("cannot send RequestPayment message: chat helper is nil")
	}

	recipient, err := LookupRecipient(m, arg.To, isCLI)
	if err != nil {
		return ret, err
	}

	post := stellar1.RequestPost{
		Amount:   arg.Amount,
		Asset:    arg.Asset,
		Currency: arg.Currency,
	}

	if recipient.User != nil {
		post.ToAssertion = recipient.User.Username.String()
		post.ToUser = &recipient.User.UV
	} else if recipient.Assertion != nil {
		post.ToAssertion = recipient.Assertion.String()
	} else {
		return ret, fmt.Errorf("expected username or user assertion as recipient")
	}

	requestID, err := remoter.SubmitRequest(m.Ctx(), post)
	if err != nil {
		return ret, err
	}

	body := chat1.NewMessageBodyWithRequestpayment(chat1.MessageRequestPayment{
		RequestID: requestID,
		Note:      arg.Note,
	})

	displayName := strings.Join([]string{m.CurrentUsername().String(), post.ToAssertion}, ",")

	membersType := chat1.ConversationMembersType_IMPTEAMNATIVE
	err = m.G().ChatHelper.SendMsgByName(m.Ctx(), displayName, nil,
		membersType, keybase1.TLFIdentifyBehavior_CHAT_SKIP, body, chat1.MessageType_REQUESTPAYMENT)
	return requestID, err
}

// Lookup a user who has the stellar account ID.
// Verifies the result against the user's sigchain.
// If there are no users, or multiple users, returns NotFoundError.
func LookupUserByAccountID(m libkb.MetaContext, accountID stellar1.AccountID) (uv keybase1.UserVersion, un libkb.NormalizedUsername, err error) {
	defer m.CTraceTimed(fmt.Sprintf("Stellar.LookupUserByAccount(%v)", accountID), func() error { return err })()
	usersUnverified, err := remote.LookupUnverified(m.Ctx(), m.G(), accountID)
	if err != nil {
		return uv, un, err
	}
	m.CDebugf("got %v unverified results", len(usersUnverified))
	for i, uv := range usersUnverified {
		m.CDebugf("usersUnverified[%v] = %v", i, uv)
	}
	if len(usersUnverified) == 0 {
		return uv, un, libkb.NotFoundError{Msg: fmt.Sprintf("No user found with account %v", accountID)}
	}
	if len(usersUnverified) > 1 {
		return uv, un, libkb.NotFoundError{Msg: fmt.Sprintf("Multiple users found with account: %v", accountID)}
	}
	uv = usersUnverified[0]
	// Verify that `uv` (from server) matches `accountID`.
	verify := func(forcePoll bool) (upak *keybase1.UserPlusKeysV2AllIncarnations, retry bool, err error) {
		defer m.CTraceTimed(fmt.Sprintf("verify(forcePoll:%v, accountID:%v, uv:%v)", forcePoll, accountID, uv), func() error { return err })()
		upak, _, err = m.G().GetUPAKLoader().LoadV2(
			libkb.NewLoadUserArgWithMetaContext(m).WithPublicKeyOptional().WithUID(uv.Uid).WithForcePoll(forcePoll))
		if err != nil {
			return nil, false, err
		}
		genericErr := errors.New("error verifying account lookup")
		if !upak.Current.EldestSeqno.Eq(uv.EldestSeqno) {
			m.CDebugf("user %v's eldest seqno did not match %v != %v", upak.Current.Username, upak.Current.EldestSeqno, uv.EldestSeqno)
			return nil, true, genericErr
		}
		if upak.Current.StellarAccountID == nil {
			m.CDebugf("user %v has no stellar account", upak.Current.Username)
			return nil, true, genericErr
		}
		unverifiedAccountID, err := libkb.ParseStellarAccountID(*upak.Current.StellarAccountID)
		if err != nil {
			m.CDebugf("user has invalid account ID '%v': %v", *upak.Current.StellarAccountID, err)
			return nil, false, genericErr
		}
		if !unverifiedAccountID.Eq(accountID) {
			m.CDebugf("user %v has different account %v != %v", upak.Current.Username, unverifiedAccountID, accountID)
			return nil, true, genericErr
		}
		return upak, false, nil
	}
	upak, retry, err := verify(false)
	if err == nil {
		return upak.Current.ToUserVersion(), libkb.NewNormalizedUsername(upak.Current.GetName()), err
	}
	if !retry {
		return keybase1.UserVersion{}, "", err
	}
	// Try again with ForcePoll in case the previous attempt lost a race.
	upak, _, err = verify(true)
	if err != nil {
		return keybase1.UserVersion{}, "", err
	}
	return upak.Current.ToUserVersion(), libkb.NewNormalizedUsername(upak.Current.GetName()), err
}

// AccountExchangeRate returns the exchange rate for an account for the logged in user.
// Note that it is possible that multiple users can own the same account and have
// different display currency preferences.
func AccountExchangeRate(mctx libkb.MetaContext, remoter remote.Remoter, accountID stellar1.AccountID) (stellar1.OutsideExchangeRate, error) {
	currency, err := GetCurrencySetting(mctx, remoter, accountID)
	if err != nil {
		return stellar1.OutsideExchangeRate{}, err
	}

	return remoter.ExchangeRate(mctx.Ctx(), string(currency.Code))
}

func RefreshUnreadCount(g *libkb.GlobalContext, accountID stellar1.AccountID) {
	g.Log.Debug("RefreshUnreadCount for stellar account %s", accountID)
	s := getGlobal(g)
	ctx := context.Background()
	details, err := s.remoter.Details(ctx, accountID)
	if err != nil {
		return
	}
	g.Log.Debug("RefreshUnreadCount got details for stellar account %s", accountID)

	s.UpdateUnreadCount(ctx, accountID, details.UnreadPayments)

	g.Log.Debug("RefreshUnreadCount UpdateUnreadCount => %d for stellar account %s", details.UnreadPayments, accountID)
}

// Get a per-user key.
// Wait for attempt but only warn on error.
func perUserKeyUpgradeSoft(ctx context.Context, g *libkb.GlobalContext, reason string) {
	m := libkb.NewMetaContext(ctx, g)
	arg := &engine.PerUserKeyUpgradeArgs{}
	eng := engine.NewPerUserKeyUpgrade(g, arg)
	err := engine.RunEngine2(m, eng)
	if err != nil {
		m.CDebugf("PerUserKeyUpgrade failed (%s): %v", reason, err)
	}
}

func HasAcceptedDisclaimer(ctx context.Context, g *libkb.GlobalContext) (bool, error) {
	return getGlobal(g).hasAcceptedDisclaimer(ctx)
}

func InformAcceptedDisclaimer(ctx context.Context, g *libkb.GlobalContext) {
	getGlobal(g).informAcceptedDisclaimer(ctx)
}

func RandomBuildPaymentID() (stellar1.BuildPaymentID, error) {
	randBytes, err := libkb.RandBytes(15)
	if err != nil {
		return "", err
	}
	return stellar1.BuildPaymentID("bb" + hex.EncodeToString(randBytes)), nil
}
