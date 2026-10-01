package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wltechblog/gino/internal/cron"

	"github.com/wltechblog/gino/internal/mcp"
)

// CronTool schedules delayed/recurring tasks via the cron scheduler.
// It holds a channel/chatID context (set per-incoming-message) so fired jobs
// know where to send their notification.
type CronTool struct {
	scheduler *cron.Scheduler
	channel   string
	chatID    string
}

func NewCronTool(scheduler *cron.Scheduler) *CronTool {
	return &CronTool{scheduler: scheduler}
}

func (t *CronTool) Name() string { return "cron" }
func (t *CronTool) Description() string {
	return "Schedule one-time or recurring reminders/tasks. Actions: add (delay-based), schedule (cron-expression), list (show pending), cancel (remove by name)."
}

func (t *CronTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "The action: add (delay-based job), schedule (cron-expression job), list (show pending jobs), cancel (remove a job by name)",
				"enum":        []string{"add", "schedule", "list", "cancel"},
			},
			"name": map[string]interface{}{
				"type":        "string",
				"description": "A short name for the job (used to identify it for cancellation)",
			},
			"message": map[string]interface{}{
				"type":        "string",
				"description": "The reminder message or task description to deliver when the job fires",
			},
			// --- delay-based (action=add) ---
			"delay": map[string]interface{}{
				"type":        "string",
				"description": "[add] How long to wait before first firing, e.g. '2m', '1h30m', '30s', '1h'. Uses Go duration format.",
			},
			"recurring": map[string]interface{}{
				"type":        "boolean",
				"description": "[add] If true, the job will repeat at the specified interval. If false or omitted, fires only once.",
			},
			"interval": map[string]interface{}{
				"type":        "string",
				"description": "[add] For recurring jobs: how often to repeat (minimum 2m). Uses Go duration format.",
			},
			// --- cron-expression (action=schedule) ---
			"cron": map[string]interface{}{
				"type":        "string",
				"description": "[schedule] Standard 5-field cron expression: \"minute hour day-of-month month day-of-week\".\nExamples:\n  \"*/15 9-16 * * 1-5\" — every 15 min during 9AM-4PM, Mon-Fri\n  \"0 8 * * 1-5\" — 8AM every weekday\n  \"0 17 * * 1-5\" — 5PM every weekday\n  \"30 9 * * 1-5\" — 9:30AM every weekday\n  \"0 0 1 1 *\" — midnight on January 1st\n  \"0,30 * * * *\" — at :00 and :30 every hour\nSupported: * (wildcard), */N (step), A-B (range), A,B,C (list)\nDay-of-week: 0=Sun, 1=Mon, ..., 6=Sat, 7=Sun",
			},
			"timezone": map[string]interface{}{
				"type":        "string",
				"description": "[schedule] IANA timezone for the cron expression, e.g. \"America/New_York\", \"Europe/London\", \"UTC\". Defaults to UTC if omitted.",
			},
		},
		"required": []string{"action"},
	}
}

// SetContext sets the originating channel and chat for scheduled jobs.
func (t *CronTool) SetContext(channel, chatID string) {
	t.channel = channel
	t.chatID = chatID
}

// resolveOrigin returns the destination a job scheduled in this turn should
// fire at. Context wins (per-turn, race-free); ambient fields are the
// fallback for callers without an origin-carrying context.
func (t *CronTool) resolveOrigin(ctx context.Context) (string, string, string) {
	if o, ok := mcp.OriginFromContext(ctx); ok && o.Channel != "" && o.ChatID != "" {
		return o.Channel, o.ChatID, o.SessionKey
	}
	return t.channel, t.chatID, ""
}

func (t *CronTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	// Per-turn origin beats ambient tool state (race-free under concurrent
	// dispatches); ambient SetContext remains the fallback.
	t.resolveOrigin(ctx)

	switch action {
	case "add":
		return t.executeAdd(ctx, args)
	case "schedule":
		return t.executeSchedule(ctx, args)
	case "list":
		return t.executeList(args)
	case "cancel":
		return t.executeCancel(args)
	default:
		return "", fmt.Errorf("cron: unknown action %q (use add, schedule, list, or cancel)", action)
	}
}

func (t *CronTool) executeAdd(ctx context.Context, args map[string]interface{}) (string, error) {
	channel, chatID, sessionKey := t.resolveOrigin(ctx)
	name, _ := args["name"].(string)
	message, _ := args["message"].(string)
	delayStr, _ := args["delay"].(string)
	recurring, _ := args["recurring"].(bool)
	intervalStr, _ := args["interval"].(string)

	if name == "" {
		name = "reminder"
	}
	if message == "" {
		return "", fmt.Errorf("cron add: 'message' is required")
	}
	if delayStr == "" {
		return "", fmt.Errorf("cron add: 'delay' is required (e.g. '2m', '1h')")
	}

	delay, err := time.ParseDuration(delayStr)
	if err != nil {
		return "", fmt.Errorf("cron add: invalid delay %q: %v", delayStr, err)
	}
	if delay <= 0 {
		return "", fmt.Errorf("cron add: delay must be positive")
	}

	if recurring {
		if intervalStr == "" {
			intervalStr = delayStr
		}
		interval, err := time.ParseDuration(intervalStr)
		if err != nil {
			return "", fmt.Errorf("cron add: invalid interval %q: %v", intervalStr, err)
		}
		if interval < 2*time.Minute {
			return "", fmt.Errorf("cron add: recurring interval must be at least 2m (got %v)", interval)
		}
		id := t.scheduler.AddRecurring(name, message, interval, channel, chatID, sessionKey)
		return fmt.Sprintf("Scheduled recurring job %q (id: %s). Will fire in %v, then repeat every %v.", name, id, delay, interval), nil
	}

	id := t.scheduler.Add(name, message, delay, channel, chatID, sessionKey)
	return fmt.Sprintf("Scheduled job %q (id: %s). Will fire in %v.", name, id, delay), nil
}

func (t *CronTool) executeSchedule(ctx context.Context, args map[string]interface{}) (string, error) {
	channel, chatID, sessionKey := t.resolveOrigin(ctx)
	name, _ := args["name"].(string)
	message, _ := args["message"].(string)
	cronExpr, _ := args["cron"].(string)
	timezone, _ := args["timezone"].(string)

	if name == "" {
		name = "scheduled"
	}
	if message == "" {
		return "", fmt.Errorf("cron schedule: 'message' is required")
	}
	if cronExpr == "" {
		return "", fmt.Errorf("cron schedule: 'cron' expression is required (e.g. \"*/15 9-16 * * 1-5\")")
	}

	id, err := t.scheduler.AddScheduled(name, message, cronExpr, timezone, channel, chatID, sessionKey)
	if err != nil {
		return "", err
	}

	// Build a human-readable description of the next fire time.
	jobs := t.scheduler.List()
	var nextFire string
	for _, j := range jobs {
		if j.ID == id {
			remaining := time.Until(j.FireAt).Round(time.Second)
			nextFire = fmt.Sprintf(", next fire in %v (%s)", remaining, j.FireAt.Format("2006-01-02 15:04 MST"))
			break
		}
	}

	return fmt.Sprintf("Scheduled cron job %q (id: %s).\nExpression: %s\nTimezone: %s%s", name, id, cronExpr, timezone, nextFire), nil
}

func (t *CronTool) executeList(args map[string]interface{}) (string, error) {
	jobs := t.scheduler.List()
	if len(jobs) == 0 {
		return "No pending jobs.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d pending job(s):\n", len(jobs))
	for _, j := range jobs {
		remaining := time.Until(j.FireAt).Round(time.Second)
		if j.Schedule != "" {
			tz := j.Timezone
			if tz == "" {
				tz = "UTC"
			}
			fmt.Fprintf(&sb, "- %s (%s): %q — cron: %q (tz: %s), next fire in %v\n", j.Name, j.ID, j.Message, j.Schedule, tz, remaining)
		} else if j.Recurring {
			fmt.Fprintf(&sb, "- %s (%s): %q — recurring every %v, fires in %v\n", j.Name, j.ID, j.Message, j.Interval, remaining)
		} else {
			fmt.Fprintf(&sb, "- %s (%s): %q — fires in %v\n", j.Name, j.ID, j.Message, remaining)
		}
	}
	return sb.String(), nil
}

func (t *CronTool) executeCancel(args map[string]interface{}) (string, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "", fmt.Errorf("cron cancel: 'name' is required")
	}
	if t.scheduler.CancelByName(name) {
		return fmt.Sprintf("Cancelled job %q.", name), nil
	}
	return fmt.Sprintf("No job found with name %q.", name), nil
}
