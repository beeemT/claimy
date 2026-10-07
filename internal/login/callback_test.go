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
	closeCallbackResponse(t, response)
	if response.StatusCode == http.StatusOK {
		t.Fatal("wrong callback path was accepted")
	}
	response, err = client.Get("http://" + callback.listener.Addr().String() + "/callback?state=wrong-state&code=bad")
	if err != nil {
		t.Fatal(err)
	}
	closeCallbackResponse(t, response)
	if response.StatusCode == http.StatusOK {
		t.Fatal("wrong callback state was accepted")
	}

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
	body, err := io.ReadAll(response.Body)
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
