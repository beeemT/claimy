package login

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
)

func TestCallbackRejectsWrongPathAndStateWithoutConsumingValidCallback(t *testing.T) {
	callback, err := listenCallback("expected-state", "http://issuer.test")
	if err != nil {
		t.Fatal(err)
	}
	cleanupCallback(t, callback)

	client := &http.Client{}
	response, err := client.Get("http://" + callback.listener.Addr().String() + "/favicon.ico?state=expected-state&code=bad")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong callback path status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("wrong callback path content type = %q", got)
	}
	closeCallbackResponse(t, response)
	response, err = client.Get("http://" + callback.listener.Addr().String() + "/callback?state=wrong-state&code=bad")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong callback state status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("wrong callback state content type = %q", got)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != response.ContentLength {
		t.Fatalf("rejected callback response: received %d bytes, expected %d", len(body), response.ContentLength)
	}
	closeCallbackResponse(t, response)

	idleConnection, err := net.Dial("tcp", callback.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := idleConnection.Close(); err != nil {
			t.Error(err)
		}
	})

	response, err = client.Get("http://" + callback.listener.Addr().String() + "/callback?state=expected-state&code=good")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid callback status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("valid callback content type = %q", got)
	}
	result, err := callback.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.code != "good" {
		t.Fatalf("callback code = %q", result.code)
	}
	if err := callback.Shutdown(); err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	closeCallbackResponse(t, response)
	if int64(len(body)) != response.ContentLength {
		t.Fatalf("callback response: received %d bytes, expected %d", len(body), response.ContentLength)
	}
}

func TestCallbackCancellationStopsWaitAndServer(t *testing.T) {
	callback, err := listenCallback("state", "http://issuer.test")
	if err != nil {
		t.Fatal(err)
	}
	cleanupCallback(t, callback)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := callback.Wait(ctx); err == nil {
		t.Fatal("canceled callback wait succeeded")
	}
	if err := callback.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

func cleanupCallback(t *testing.T, callback *callbackListener) {
	t.Helper()
	t.Cleanup(func() {
		if err := callback.Shutdown(); err != nil {
			t.Error("stop callback server")
		}
	})
}

func closeCallbackResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Error("close callback response body")
	}
}
