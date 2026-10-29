package lifecycle

import (
	"net/http"
	"time"
)

// The lease outlives each request; its time and pinned files are checked before
// sending and before returning a response. An API call cannot extend a window.
type credentialTransport struct {
	base       http.RoundTripper
	credential *sessionCredential
	now        func() time.Time
}

func (c *sessionCredential) accepts(r request) bool {
	if c == nil || c.controller != "" && c.controller != r.Controller {
		return false
	}
	if c.store != nil && c.store.raw["sign.intent.json"] != nil {
		return r.RepositoryID == 1432619931 && r.Owner == "headironc" && r.Repository == "nucleus"
	}
	return true
}

func (t credentialTransport) valid() bool {
	c := t.credential
	if c == nil || c.store == nil || t.now == nil || t.now().Unix() >= c.expires || !c.store.unchanged() {
		return false
	}
	if raw := c.store.raw["request.json"]; raw != nil && c.store.raw["sign.intent.json"] != nil {
		p, err := decodeObserverPlan(raw)
		if err != nil || t.now().Unix() >= p.RequiredUntil || observerClock(c.store, t.now().Unix()) != nil {
			return false
		}
	}
	if raw := c.store.raw["intent.json"]; raw != nil {
		var i sessionIntent
		if strictObject(raw, &i) != nil || t.now().Unix() < i.Started {
			return false
		}
	}
	return true
}

func (t credentialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.valid() {
		return nil, errRefused
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if !t.valid() {
		response.Body.Close()
		return nil, errRefused
	}
	return response, nil
}
