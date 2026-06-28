//go:build only_telegram

package channels

import (
	"context"
	"log"

	"github.com/wltechblog/gino/internal/chat"
)

func StartDiscord(ctx context.Context, hub *chat.Hub, token string, allowFrom []string, allowDMs bool, monitorChannels []string, rl DiscordRateLimit) error {
	log.Println("discord: channel not compiled into this binary (built with single-channel tag).")
	return nil
}
