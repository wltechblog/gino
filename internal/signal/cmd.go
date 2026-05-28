package signal

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// NewSendCmd returns a cobra command that sends a signal to a picobot Unix socket.
// Usage: picobot signal send --source my-script --action motion_detected
func NewSendCmd() *cobra.Command {
	var (
		sigSource   string
		sigAction   string
		sigChannel  string
		sigChatID   string
		socketPath  string
	)

	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send an external signal to a running picobot gateway",
		Run: func(cmd *cobra.Command, args []string) {
			if sigAction == "" {
				fmt.Fprintln(os.Stderr, "action is required (--action or -a)")
				os.Exit(1)
			}
			if sigSource == "" {
				fmt.Fprintln(os.Stderr, "source is required (--source or -s)")
				os.Exit(1)
			}

			sig := Signal{
				Source:  sigSource,
				Action:  sigAction,
				Channel: sigChannel,
				ChatID:  sigChatID,
			}

			if err := SendSignal(socketPath, sig); err != nil {
				fmt.Fprintf(os.Stderr, "failed to send signal: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Signal sent: source=%s action=%s\n", sigSource, sigAction)
		},
	}

	cmd.Flags().StringVarP(&sigSource, "source", "s", "", "Source identifier (e.g., my-script, camera-1) (required)")
	cmd.Flags().StringVarP(&sigAction, "action", "a", "", "Registered action name (required)")
	cmd.Flags().StringVarP(&sigChannel, "channel", "c", "", "Target channel (e.g., telegram, discord)")
	cmd.Flags().StringVarP(&sigChatID, "chat-id", "", "", "Target chat ID")
	cmd.Flags().StringVarP(&socketPath, "socket", "", "", "Unix socket path (default: {workspace}/.picobot/signals.sock)")

	return cmd
}

// FormatSignalJSON returns a pretty-printed JSON representation of a signal.
func FormatSignalJSON(sig Signal) string {
	b, _ := json.MarshalIndent(sig, "", "  ")
	return string(b)
}
