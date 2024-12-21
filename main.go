package main

import (
	"TeleURLUploader/progress"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

func main() {
	client, err := gotgproto.NewClient(
		ApiID,
		ApiHash,
		gotgproto.ClientTypeBot(BotToken),
		&gotgproto.ClientOpts{
			Session: sessionMaker.SqlSession(sqlite.Open(SessionPath)),
		},
	)
	if err != nil {
		log.Fatalln(err)
	}

	client.Dispatcher.AddHandler(handlers.NewMessage(filters.Message.Text, receivedMsg))

	log.Println("starting!")
	client.Idle()
}

func receivedMsg(ctx *ext.Context, update *ext.Update) error {
	if !slices.Contains(Authorized, update.EffectiveUser().GetID()) || !update.EffectiveChat().IsAUser() {
		return nil
	} else if update.EffectiveMessage.Text == "/start" {
		ctx.Reply(update, ext.ReplyTextString("welcome! just send a url to download!"), nil)
		return nil
	}

	chatID := update.EffectiveChat().GetID()
	spl := strings.SplitN(update.EffectiveMessage.Text, " ", 2)
	link := spl[0]

	if _, err := url.ParseRequestURI(link); err != nil {
		return nil // just some random text, ignored
	}

	sent, err := ctx.Reply(update, ext.ReplyTextString("downloading.."), nil)
	if err != nil {
		return err
	}

	resp, err := http.Get(link)
	if err != nil {
		ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
			ID:      sent.ID,
			Message: "error during GET: " + err.Error(),
		})
		return err
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
			ID:      sent.ID,
			Message: "got non-2xx status code: " + resp.Status,
		})
		return nil
	}
	defer resp.Body.Close()

	proxy, err := progress.NewProxy(ctx, update, sent, float64(resp.ContentLength))
	if err != nil {
		ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
			ID:      sent.ID,
			Message: "error while making proxy: " + err.Error(),
		})
		return err
	}
	defer proxy.DeleteTemp()

	for {
		if _, err := io.CopyN(proxy, resp.Body, progress.MB); err != nil {
			if errors.Is(err, io.EOF) { // done downloading
				break
			}

			ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
				ID:      sent.ID,
				Message: "error while downloading: " + err.Error(),
			})
			return err
		}
	}

	if err = proxy.PrepareToUpload(); err != nil {
		ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
			ID:      sent.ID,
			Message: "error while preparing to upload: " + err.Error(),
		})
		return err
	}

	ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
		ID:      sent.ID,
		Message: "uploading..",
	})

	var filename string
	if len(spl) == 2 {
		filename = spl[1] // we used SplitN, so only 2 elements in the split !
	} else {
		filename = GetFilename(link, resp.Header.Get("content-disposition"), resp.Header.Get("content-type"))
	}

	uploaded, err := uploader.NewUploader(ctx.Raw).WithPartSize(524288).FromReader(ctx, filename, proxy)
	if err != nil {
		ctx.EditMessage(chatID, &tg.MessagesEditMessageRequest{
			ID:      sent.ID,
			Message: "error while uploading: " + err.Error(),
		})
		return errors.New("error while making uploader: " + err.Error())
	}

	_, err = ctx.SendMedia(chatID, &tg.MessagesSendMediaRequest{
		ReplyTo: &tg.InputReplyToMessage{
			ReplyToMsgID: update.EffectiveMessage.ID,
			ReplyToPeerID: &tg.InputPeerUser{
				UserID: update.EffectiveUser().GetID(),
			},
		},
		Media: &tg.InputMediaUploadedDocument{
			File: uploaded,
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{
					FileName: filename,
				},
			},
		},
	})
	if err != nil {
		return errors.New("error while sending media: " + err.Error())
	}

	ctx.DeleteMessages(chatID, []int{sent.ID})

	return nil
}
