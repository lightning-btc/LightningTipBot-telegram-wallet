package telegram

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/dalle"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	log "github.com/sirupsen/logrus"
	"github.com/skip2/go-qrcode"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// generateImages is called when the user enters /generate or /generate <prompt>
// asks the user for a prompt if not given
func (bot *TipBot) generateImages(ctx intercept.Context) (intercept.Context, error) {
	bot.anyTextHandler(ctx)
	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, fmt.Errorf("user has no wallet")
	}

	if len(strings.Split(ctx.Message().Text, " ")) < 2 {
		// We need to save the pay state in the user state so we can load the payment in the next handler
		SetUserState(user, bot, lnbits.UserEnterDallePrompt, "")
		bot.trySendMessage(ctx.Message().Sender, "⌨️ Enter image prompt.", tb.ForceReply)
		return ctx, nil
	}
	// write the prompt into the command and call confirm
	m := ctx.Message()
	m.Text = GetMemoFromCommand(m.Text, 1)
	return bot.confirmGenerateImages(ctx)
}

// confirmGenerateImages is called when the user has entered a prompt through /generate <prompt>
// or because he answered to the request to enter it in generateImages()
// confirmGenerateImages will create an invoice that the user can pay and if they pay
// generateDalleImages will fetch the images and send it to the user
func (bot *TipBot) confirmGenerateImages(ctx intercept.Context) (intercept.Context, error) {
	user := LoadUser(ctx)

	ResetUserState(user, bot)
	m := ctx.Message()
	prompt := m.Text
	if len(prompt) == 0 {
		return ctx, fmt.Errorf("prompt not given")
	}

	if user.Wallet == nil {
		return ctx, fmt.Errorf("user has no wallet")
	}
	me, err := GetUser(bot.Telegram.Me, *bot)
	if err != nil {
		return ctx, err
	}
	invoice, err := bot.createInvoiceWithEvent(ctx, me, internal.Configuration.Generate.DallePrice, fmt.Sprintf("DALLE2 %s", GetUserStr(user.Telegram)), InvoiceCallbackGenerateDalle, prompt)
	invoice.Payer = user
	if err != nil {
		return ctx, err
	}

	runtime.IgnoreError(bot.Bunt.Set(invoice))

	balance, err := bot.GetUserBalance(user)
	if err != nil {
		errmsg := fmt.Sprintf("[inlineReceive] Error: Could not get user balance: %s", err.Error())
		log.Warnln(errmsg)
	}

	bot.trySendMessage(ctx.Message().Sender, Translate(ctx, "generateDallePayInvoiceMessage"))

	// invoke internal pay if enough balance
	if balance >= internal.Configuration.Generate.DallePrice {
		m.Text = fmt.Sprintf("/pay %s", invoice.PaymentRequest)
		return bot.payHandler(ctx)
	}

	// create qr code
	qr, err := qrcode.Encode(invoice.PaymentRequest, qrcode.Medium, 256)
	if err != nil {
		bot.tryEditMessage(invoice.Message, Translate(ctx, "errorTryLaterMessage"))
		return ctx, err
	}

	// send the invoice data to user
	msg := bot.trySendMessage(ctx.Message().Sender, &tb.Photo{File: tb.File{FileReader: bytes.NewReader(qr)}, Caption: fmt.Sprintf("`%s`", invoice.PaymentRequest)})
	invoice.InvoiceMessage = msg
	runtime.IgnoreError(bot.Bunt.Set(invoice))
	return ctx, nil
}

// generateDalleImages is called by the invoice event when the user has paid
func (bot *TipBot) generateDalleImages(event Event) {
	invoiceEvent := event.(*InvoiceEvent)
	user := invoiceEvent.Payer
	if user == nil || user.Wallet == nil {
		log.Errorf("[generateDalleImages] invalid user")
		return
	}

	bot.trySendMessage(user.Telegram, "Your images are being generated. Please wait...")

	// create the client with the bearer token api key
	dalleClient, err := dalle.NewHTTPClient(internal.Configuration.Generate.DalleKey)
	// handle err
	if err != nil {
		log.Errorf("[NewHTTPClient] %v", err.Error())
		bot.dalleRefundUser(user)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancel()
	// generate a task to create an image with a prompt
	task, err := dalleClient.Generate(ctx, invoiceEvent.CallbackData)
	if err != nil {
		log.Errorf("[Generate] %v", err.Error())
		bot.dalleRefundUser(user)
		return
	}
	// poll the task.ID until status is succeeded
	var t *dalle.Task
	for {
		time.Sleep(time.Second * 3)

		t, err = dalleClient.GetTask(ctx, task.ID)
		// handle err
		if err != nil {
			log.Errorf("[GetTask] %v", err.Error())
			bot.dalleRefundUser(user)
			return
		}
		if t.Status == dalle.StatusSucceeded {
			fmt.Printf("[DALLE] task succeeded for user %s", GetUserStr(user.Telegram))
			break
		} else if t.Status == dalle.StatusRejected {
			log.Errorf("[DALLE] rejected: %s", t.ID)
			bot.dalleRefundUser(user)
			break
		}

		log.Debugf("[DALLE] pending for user %s", GetUserStr(user.Telegram))
	}

	// download the first generated image
	for _, data := range t.Generations.Data {
		err = bot.downloadAndSendImages(ctx, dalleClient, data, invoiceEvent)
		if err != nil {
			log.Errorf("[downloadAndSendImages] %v", err.Error())
		}
	}

	// handle err and close readCloser
}

// downloadAndSendImages will download dalle images and send them to the payer.
func (bot *TipBot) downloadAndSendImages(ctx context.Context, dalleClient dalle.Client, data dalle.GenerationData, event *InvoiceEvent) error {
	reader, err := dalleClient.Download(ctx, data.ID)
	if err != nil {
		return err
	}
	defer reader.Close()
	image := "data/dalle/" + data.ID + ".png"
	file, err := os.Create(image)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(image, 0, os.ModePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	bot.trySendMessage(event.Payer.Telegram, &tb.Photo{File: tb.File{FileReader: f}})
	return nil
}

func (bot *TipBot) dalleRefundUser(user *lnbits.User) error {
	if user.Wallet == nil {
		return fmt.Errorf("user has no wallet")
	}
	me, err := GetUser(bot.Telegram.Me, *bot)
	if err != nil {
		return err
	}

	// create invioce for user
	invoice, err := user.Wallet.Invoice(
		lnbits.InvoiceParams{
			Out:     false,
			Amount:  int64(internal.Configuration.Generate.DallePrice),
			Memo:    "Refund for /generate",
			Webhook: internal.Configuration.Lnbits.WebhookServer},
		bot.Client)
	if err != nil {
		return err
	}

	// pay invoice
	_, err = me.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: invoice.PaymentRequest}, bot.Client)
	if err != nil {
		log.Errorln(err)
		return err
	}
	return nil
}
