package login

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	maxCallbackQueryBytes  = 4096
	callbackFailureInvalid = "invalid"
)

var callbackServerTimeout = 2 * time.Second

type callbackResult struct {
	code    string
	failure string
}

type callbackListener struct {
	redirectURL string
	listener    net.Listener
	server      *http.Server
	results     chan callbackResult
	done        chan struct{}

	mu          sync.Mutex
	serveErr    error
	accepted    bool
	shutdown    sync.Once
	shutdownErr error
}

func listenCallback(state, issuer string) (*callbackListener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	callback := &callbackListener{
		redirectURL: "http://" + listener.Addr().String() + "/callback",
		listener:    listener,
		results:     make(chan callbackResult, 1),
		done:        make(chan struct{}),
	}
	callback.server = &http.Server{
		Handler:           http.HandlerFunc(callback.handler(state, issuer)),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       2 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go func() {
		err := callback.server.Serve(listener)
		callback.mu.Lock()
		callback.serveErr = err
		callback.mu.Unlock()
		close(callback.done)
	}()

	return callback, nil
}

func (callback *callbackListener) handler(expectedState, expectedIssuer string) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		query, status := parseCallbackRequest(request, expectedState)
		if status != 0 {
			rejectCallback(response, status)

			return
		}
		if !callback.accept() {
			rejectCallback(response, http.StatusGone)

			return
		}

		callback.results <- callbackResultForQuery(query, expectedIssuer)
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		response.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprintln(response, "Authorization callback received. Login is being verified."); err != nil {
			return
		}
	}
}

func parseCallbackRequest(request *http.Request, expectedState string) (url.Values, int) {
	if request.Method != http.MethodGet {
		return nil, http.StatusMethodNotAllowed
	}
	if request.URL == nil {
		return nil, http.StatusBadRequest
	}
	if request.URL.Path != "/callback" || request.URL.RawPath != "" && request.URL.RawPath != "/callback" {
		return nil, http.StatusNotFound
	}
	if len(request.URL.RawQuery) > maxCallbackQueryBytes {
		return nil, http.StatusRequestURITooLong
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return nil, http.StatusBadRequest
	}
	states := query["state"]
	if len(states) != 1 || subtle.ConstantTimeCompare([]byte(states[0]), []byte(expectedState)) != 1 {
		return nil, http.StatusBadRequest
	}

	return query, 0
}

func (callback *callbackListener) accept() bool {
	callback.mu.Lock()
	defer callback.mu.Unlock()
	if callback.accepted {
		return false
	}
	callback.accepted = true

	return true
}

func callbackResultForQuery(query url.Values, expectedIssuer string) callbackResult {
	result := callbackResult{}
	codes, hasCode := query["code"]
	errorsFromProvider, hasError := query["error"]
	issuers, hasIssuer := query["iss"]
	switch {
	case hasIssuer && (len(issuers) != 1 || issuers[0] != expectedIssuer):
		result.failure = callbackFailureInvalid
	case hasError && (len(errorsFromProvider) != 1 || errorsFromProvider[0] == ""):
		result.failure = callbackFailureInvalid
	case hasError && hasCode:
		result.failure = callbackFailureInvalid
	case hasError:
		result.failure = "authorization"
	case !hasCode || len(codes) != 1 || strings.TrimSpace(codes[0]) == "":
		result.failure = callbackFailureInvalid
	default:
		result.code = codes[0]
	}

	return result
}

func rejectCallback(response http.ResponseWriter, status int) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(status)
	if _, err := fmt.Fprintln(response, "This is not a valid login callback. Return to the Claimy CLI."); err != nil {
		return
	}
}

func (callback *callbackListener) Wait(ctx context.Context) (callbackResult, error) {
	select {
	case result := <-callback.results:
		return result, nil
	case <-callback.done:
		if isNormalServerClose(callback.getServeError()) {
			return callbackResult{}, errors.New("login callback server stopped")
		}

		return callbackResult{}, errors.New("login callback server failed")
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	}
}

func (callback *callbackListener) getServeError() error {
	callback.mu.Lock()
	defer callback.mu.Unlock()

	return callback.serveErr
}

func isNormalServerClose(err error) bool {
	return errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)
}

func (callback *callbackListener) Shutdown() error {
	callback.shutdown.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), callbackServerTimeout)
		defer cancel()
		var shutdownErrors []error
		if err := callback.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			shutdownErrors = append(shutdownErrors, errors.New("stop login callback server failed"))
			if err := closeCallbackServer(callback); err != nil {
				shutdownErrors = append(shutdownErrors, err)
			}
		}
		if err := closeCallbackListener(callback); err != nil {
			shutdownErrors = append(shutdownErrors, err)
		}

		select {
		case <-callback.done:
			if err := callback.getServeError(); err != nil && !isNormalServerClose(err) {
				shutdownErrors = append(shutdownErrors, errors.New("login callback server failed"))
			}
		case <-ctx.Done():
			shutdownErrors = append(shutdownErrors, errors.New("stop login callback server failed"))
			if err := closeCallbackServer(callback); err != nil {
				shutdownErrors = append(shutdownErrors, err)
			}
			if err := closeCallbackListener(callback); err != nil {
				shutdownErrors = append(shutdownErrors, err)
			}
		}
		callback.shutdownErr = errors.Join(shutdownErrors...)
	})

	return callback.shutdownErr
}

func closeCallbackServer(callback *callbackListener) error {
	if err := callback.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return errors.New("close login callback server failed")
	}

	return nil
}

func closeCallbackListener(callback *callbackListener) error {
	if err := callback.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return errors.New("close login callback listener failed")
	}

	return nil
}
