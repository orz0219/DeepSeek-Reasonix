package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// runWebhook 启动飞书 Webhook 模式。
func (a *adapter) runWebhook(ctx context.Context) {
	port := a.cfg.WebhookPort
	if port == 0 {
		port = 8080
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/feishu/event", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var challenge struct {
			Challenge string `json:"challenge"`
			Token     string `json:"token"`
			Type      string `json:"type"`
		}
		_ = json.Unmarshal(body, &challenge)
		if challenge.Type == "url_verification" {
			if !a.verificationTokenValid(challenge.Token) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{"challenge": challenge.Challenge}); err != nil {
				a.logger.Error("feishu challenge response error", "err", err)
			}
			return
		}

		var evt feishuEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !a.verificationTokenValid(evt.Header.Token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if !a.handleCardAction(body) {
			raw, _ := json.Marshal(evt)
			a.handleWSEvent(ctx, raw)
		}
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		if err := server.Shutdown(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("feishu webhook shutdown error", "err", err)
		}
	}()

	a.logger.Info("feishu webhook listening", "port", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		a.logger.Error("feishu webhook server error", "err", err)
	}
}
