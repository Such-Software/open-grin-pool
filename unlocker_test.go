package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

var errNotFound = errors.New("block not found")

// A node that answers v2 foreign calls the way grin does, wrapping results in
// {"result":{"Ok":...}} and errors in {"result":{"Err":...}}.
func fakeNode(t *testing.T, handler func(method string, params []interface{}) (interface{}, error)) *config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out, err := handler(req.Method, req.Params)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"result": map[string]interface{}{"Err": err.Error()},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"result": map[string]interface{}{"Ok": out},
		})
	}))
	t.Cleanup(srv.Close)

	host, port := hostPortOf(t, srv.URL)
	c := &config{}
	c.Node.Address = host
	c.Node.APIPort = port
	c.Node.AuthUser = "grin"
	c.Node.AuthPass = "test"
	return c
}

func hostPortOf(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", raw, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("cannot parse port of %s: %v", raw, err)
	}
	return u.Hostname(), port
}

func TestTheNodeClientUnwrapsGrinsOkEnvelope(t *testing.T) {
	conf := fakeNode(t, func(method string, _ []interface{}) (interface{}, error) {
		if method != "get_tip" {
			t.Errorf("unexpected method %s", method)
		}
		return map[string]interface{}{"height": 4018795}, nil
	})
	h, err := newNodeAPI(conf).tipHeight()
	if err != nil {
		t.Fatal(err)
	}
	if h != 4018795 {
		t.Errorf("tip height %d, want 4018795", h)
	}
}

func TestAnErrInsideATwoHundredIsStillAFailure(t *testing.T) {
	// grin returns HTTP 200 with {"result":{"Err":...}}. Treating that as success is how a
	// watcher ends up crediting rounds against a node that told it no.
	conf := fakeNode(t, func(string, []interface{}) (interface{}, error) {
		return nil, errNotFound
	})
	if _, err := newNodeAPI(conf).tipHeight(); err == nil {
		t.Fatal("an Err envelope was accepted as success")
	}
}

func TestHeightsDecodeWithoutAnInterfaceAssertion(t *testing.T) {
	// encoding/json decodes every number to float64. Upstream asserted the header height to
	// int64 through an interface{}, which panics rather than erroring. A typed struct turns
	// a shape change into an unmarshal error instead.
	conf := fakeNode(t, func(method string, _ []interface{}) (interface{}, error) {
		return map[string]interface{}{"height": 4018795, "hash": "00038b0f"}, nil
	})
	got, err := newNodeAPI(conf).hashAtHeight(4018795)
	if err != nil {
		t.Fatal(err)
	}
	if got != "00038b0f" {
		t.Errorf("hash %q, want 00038b0f", got)
	}
}

func TestTheRewardIsTheFlatBlockRewardPlusFees(t *testing.T) {
	conf := fakeNode(t, func(method string, _ []interface{}) (interface{}, error) {
		return map[string]interface{}{
			"kernels": []map[string]interface{}{{"fee": 7000000}, {"fee": 3000000}},
		}, nil
	})
	got, err := newNodeAPI(conf).rewardAt(4018795)
	if err != nil {
		t.Fatal(err)
	}
	if want := BlockReward + 10000000; got != want {
		t.Errorf("reward %d, want %d", got, want)
	}
}

func TestBlockRewardMatchesConsensus(t *testing.T) {
	// consensus.rs: REWARD = BLOCK_TIME_SEC * GRIN_BASE, 60 seconds times 10^9. No halving.
	if BlockReward != 60_000_000_000 {
		t.Errorf("BlockReward is %d, want 60 GRIN in nanogrin", BlockReward)
	}
}
