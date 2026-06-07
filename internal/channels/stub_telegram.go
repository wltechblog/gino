//go:build only_discord

package channels

import (
	"context"
	"log"

	"github.com/local/picobot/internal/chat"
)

func StartTelegram(ctx context.Context, hub *chat.Hub, token string, allowFrom []string, showTyping bool) error {
	log.Println("telegram: channel not compiled into this binary (built with single-channel tag).")
	return nil
}
