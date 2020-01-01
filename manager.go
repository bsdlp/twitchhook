package twitchhook

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"time"
)

// SubscriptionRequest contains parameters for a subscription
type SubscriptionRequest struct {
	Topic           string
	CallbackBaseURL string
	Lease           time.Duration
}

func (r *SubscriptionRequest) validate() error {
	if r.Topic == "" {
		return errors.New("subscription topic is required")
	}

	if r.CallbackBaseURL == "" {
		return errors.New("callbackBaseURL is required")
	}

	if r.Lease == 0 {
		return errors.New("lease is required")
	}

	return nil
}

// SubscriptionID describes an id
type SubscriptionID string

// NewSubscriptionID generates a random subscription id using topic
func NewSubscriptionID(topic string) (SubscriptionID, error) {
	buf := make([]byte, 4, 4+len(topic))
	_, err := rand.Read(buf)
	if err != nil {
		return "", err
	}
	buf = append(buf, []byte(topic)...)
	return SubscriptionID(base64.URLEncoding.EncodeToString(buf)), nil
}

// SubscriptionIDToTopic converts an id to a topic
func SubscriptionIDToTopic(id SubscriptionID) (string, error) {
	bs, err := base64.URLEncoding.DecodeString(string(id))
	if err != nil {
		return "", err
	}
	return string(bs[0:3]), nil
}

// Manager takes care of webhooks
type Manager interface {
	SubscriptionCallbackHandler() http.HandlerFunc
	Subscribe(req SubscriptionRequest, deniedCallback func(reason string)) error
	Unsubscribe(topic string) error
	ValidateSignature(*http.Request) (valid bool, body io.Reader, err error)
}
