package twitchhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// TwitchWebhookHandler is an implementation for twitch webhooks
type TwitchWebhookHandler struct {
	Manager SubscriptionManager

	OAuth2ClientID     string
	OAuth2ClientSecret string

	Logger *zap.Logger

	hubURL string
	client *http.Client
	once   sync.Once
}

func (m *TwitchWebhookHandler) setup() {
	if m.client == nil {
		cfg := clientcredentials.Config{
			ClientID:     m.OAuth2ClientID,
			ClientSecret: m.OAuth2ClientSecret,
			TokenURL:     "https://id.twitch.tv/oauth2/token",
			AuthStyle:    oauth2.AuthStyleInParams,
		}

		m.client = cfg.Client(context.TODO())
	}

	if m.hubURL == "" {
		m.hubURL = "https://api.twitch.tv/helix/webhooks/hub"
	}
}

// SubscriptionCallbackHandler handles websub requests
func (m *TwitchWebhookHandler) SubscriptionCallbackHandler() http.HandlerFunc {
	return m.confirmationHandler
}

func (m *TwitchWebhookHandler) confirmationHandler(w http.ResponseWriter, r *http.Request) {
	kv := r.URL.Query()
	mode := kv.Get("hub.mode")
	if mode == "" {
		http.Error(w, "missing required hub.mode query parameter", http.StatusBadRequest)
		return
	}

	topic := kv.Get("hub.topic")
	if topic == "" {
		http.Error(w, "missing required hub.topic query parameter", http.StatusBadRequest)
		return
	}

	switch mode {
	case "denied":
		m.deniedSubHandler(w, topic, kv.Get("hub.reason"))
		return
	case "subscribe":
		m.subConfirmationHandler(w, topic, kv.Get("hub.challenge"), kv.Get("hub.lease"))
		return
	case "unsubscribe":
		m.unsubConfirmationHandler(w, topic, kv.Get("hub.challenge"))
		return
	default:
		http.Error(w, "hub.mode must be subscribe or unsubscribe", http.StatusBadRequest)
	}
	return
}

func (m *TwitchWebhookHandler) deniedSubHandler(w http.ResponseWriter, topic, reason string) {
	subscription, err := m.Manager.Get(topic)
	if err != nil {
		m.Logger.Error("error retrieving subscription from cache", zap.Error(err))
		http.Error(w, "error retrieving subscription from cache", http.StatusInternalServerError)
		return
	}

	if subscription == nil {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}

	subscription.DenialCallback(reason)

	err = m.Manager.Delete(topic)
	if err != nil {
		m.Logger.Error("error deleting subscription from cache", zap.Error(err))
		http.Error(w, "error deleting subscription from cache", http.StatusInternalServerError)
		return
	}
}

func (m *TwitchWebhookHandler) subConfirmationHandler(w http.ResponseWriter, topic, challenge, lease string) {
	if challenge == "" {
		http.Error(w, "missing required hub.challenge query parameter", http.StatusBadRequest)
		return
	}

	seconds, err := strconv.ParseInt(lease, 10, 64)
	if err != nil || seconds <= 0 {
		m.Logger.Info("received invalid lease from subscription confirmation")
		http.Error(w, "invalid lease", http.StatusBadRequest)
		return
	}

	exists, err := m.Manager.SetSubscriptionLease(topic, time.Duration(seconds)*time.Second)
	if err != nil {
		m.Logger.Error("error fetching subscription from cache", zap.Error(err))
		http.Error(w, "error fetching subscription from cache", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "subscription does not exist", http.StatusNotFound)
		return
	}

	_, err = io.WriteString(w, challenge)
	if err != nil {
		m.Logger.Info("error responding with challenge", zap.Error(err))
	}
	return
}

func (m *TwitchWebhookHandler) unsubConfirmationHandler(w http.ResponseWriter, topic, challenge string) {
	err := m.Manager.Delete(topic)
	if err != nil {
		m.Logger.Error("error deleting subscription from cache", zap.Error(err))
		http.Error(w, "error deleting subscription from cache", http.StatusInternalServerError)
		return
	}

	if challenge == "" {
		m.Logger.Info("unsub confirmation missing hub.challenge query parameter", zap.String("topic", topic))
		http.Error(w, "missing required hub.challenge query parameter", http.StatusBadRequest)
		return
	}

	_, err = io.WriteString(w, challenge)
	if err != nil {
		m.Logger.Info("error responding with challenge", zap.Error(err))
	}
	return
}

func generateCallbackURL(baseURL string, subscriptionID SubscriptionID) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	return path.Join(u.EscapedPath(), string(subscriptionID)), nil
}

// Subscribe subscribes the webhook
func (m *TwitchWebhookHandler) Subscribe(request SubscriptionRequest, denialCallback func(reason string)) (err error) {
	m.once.Do(m.setup)

	err = request.validate()
	if err != nil {
		return err
	}

	id, err := NewSubscriptionID(request.Topic)
	if err != nil {
		return err
	}

	key := make([]byte, 64)
	_, err = rand.Read(key)
	if err != nil {
		return err
	}

	callbackURL, err := generateCallbackURL(request.CallbackBaseURL, id)
	if err != nil {
		return err
	}

	subscription := &Subscription{
		Topic:          request.Topic,
		CallbackURL:    callbackURL,
		Lease:          request.Lease,
		Secret:         hex.EncodeToString(key),
		DenialCallback: denialCallback,
		Renew: func() {
			err := m.Subscribe(request, denialCallback)
			if err != nil {
				m.Logger.Error("unable to renew webhook subscription", zap.Error(err))
			}
		},
	}

	data := url.Values{}
	data.Set("hub.callback", subscription.CallbackURL)
	data.Set("hub.topic", subscription.Topic)
	data.Set("hub.lease_seconds", strconv.FormatInt(subscription.Lease.Milliseconds()/1000, 10))
	data.Set("hub.secret", subscription.Secret)
	data.Set("hub.mode", "subscribe")

	resp, err := m.client.PostForm(m.hubURL, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bs, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusAccepted {
		err = m.Manager.Save(request.Topic, subscription)
		if err != nil {
			return err
		}
		return nil
	}

	var tErr TwitchError
	err = json.Unmarshal(bs, &tErr)
	if err != nil {
		return err
	}
	return tErr
}

// Unsubscribe unsubscribes the webhook
func (m *TwitchWebhookHandler) Unsubscribe(topic string) error {
	m.once.Do(m.setup)

	subscription, err := m.Manager.Get(topic)
	if err != nil {
		return err
	}

	err = m.Manager.Delete(topic)
	if err != nil {
		return err
	}

	data := url.Values{}
	data.Set("hub.mode", "unsubscribe")
	data.Set("hub.topic", topic)
	data.Set("hub.callback", subscription.CallbackURL)

	_, err = m.client.PostForm(m.hubURL, data)
	if err != nil {
		return err
	}
	return nil
}

// ValidateSignature validates the notification using the subscription's secret
func (m *TwitchWebhookHandler) ValidateSignature(r *http.Request) (bool, io.Reader, error) {
	defer r.Body.Close()

	_, id := path.Split(r.URL.EscapedPath())
	topic, err := SubscriptionIDToTopic(SubscriptionID(id))
	if err != nil {
		return false, nil, err
	}

	subscription, err := m.Manager.Get(topic)
	if err != nil {
		return false, nil, err
	}

	bs, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return false, nil, err
	}
	body := bytes.NewReader(bs)

	hasher := hmac.New(sha256.New, []byte(subscription.Secret))
	_, err = hasher.Write(bs)
	if err != nil {
		return false, nil, err
	}

	mac := hasher.Sum(nil)

	signature := r.Header.Get("X-Hub-Signature")
	if signature == "" {
		return false, body, nil
	}

	providedMac, err := hex.DecodeString(signature)
	if err != nil {
		return false, nil, err
	}

	return hmac.Equal(mac, providedMac), body, nil
}

// TwitchError is the api message format for errors
type TwitchError struct {
	Err     string `json:"error"`
	Status  int64  `json:"status"`
	Message string `json:"message"`
}

func (e TwitchError) Error() string {
	return e.Message
}
