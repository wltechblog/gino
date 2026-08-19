package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/wltechblog/gino/internal/chat"
)

// applyWorkspace swaps the active workspace across every runtime component:
// filesystem tool roots, exec tool default cwd, and the context builder.
// It returns the canonical path applied. Called at boot (restore) and by
// runtime project switching.
func (a *AgentLoop) applyWorkspace(path string) error {
	if path == "" {
		return fmt.Errorf("empty workspace path")
	}
	if err := a.fsTool.SetWorkspace(path); err != nil {
		return fmt.Errorf("filesystem tool: %w", err)
	}
	if err := a.execTool.SetWorkspace(path); err != nil {
		// Roll the filesystem tool back so both stay consistent.
		_ = a.fsTool.SetWorkspace(a.profileWorkspace)
		return fmt.Errorf("exec tool: %w", err)
	}
	a.context.SetWorkspace(a.fsTool.WorkspaceDir())
	return nil
}

// busyTurns returns the number of active (running) turns across all sessions.
func (a *AgentLoop) busyTurns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.active)
}

// handleProjectCommand implements the /project command surface:
//
//	/project                     status + inline picker (Telegram) or text list
//	/project list                list registered projects
//	/project <name>              switch active project
//	/project none                back to profile workspace
//	/project add <name> <path>   register a project directory
//	/project remove <name>       unregister a project
func (a *AgentLoop) handleProjectCommand(msg chat.Inbound, rest string) {
	if a.projects == nil {
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, "⚠️ Project registry unavailable (check logs at startup).")
		return
	}
	switch {
	case rest == "" || rest == "status":
		a.sendProjectStatus(msg)
	case rest == "list":
		a.sendProjectList(msg)
	case rest == "none" || rest == "default" || rest == "profile":
		a.handleProjectSwitch(msg, "")
	case strings.HasPrefix(rest, "add "):
		arg := strings.TrimPrefix(rest, "add ")
		parts := strings.SplitN(arg, " ", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			sendChannelNotification(a.hub, msg.Channel, msg.ChatID, "Usage: `/project add <name> <path>`")
			return
		}
		a.handleProjectAdd(msg, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
	case strings.HasPrefix(rest, "remove "), rest == "remove":
		name := strings.TrimSpace(strings.TrimPrefix(rest, "remove"))
		if name == "" {
			sendChannelNotification(a.hub, msg.Channel, msg.ChatID, "Usage: `/project remove <name>`")
			return
		}
		a.handleProjectRemove(msg, name)
	default:
		// Treat the whole rest as a project name.
		a.handleProjectSwitch(msg, strings.Fields(rest)[0])
	}
}

// sendProjectStatus shows the active project and (on Telegram) an inline
// picker mirroring the /sessions keyboard.
func (a *AgentLoop) sendProjectStatus(msg chat.Inbound) {
	active := a.projects.ActiveProject()
	name := active
	if name == "" {
		name = "(profile workspace)"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🗂 Active project: *%s*\n", name))
	if p, ok := a.projects.Get(active); ok && active != "" {
		sb.WriteString(fmt.Sprintf("Path: `%s`\n", p.Path))
	} else {
		sb.WriteString(fmt.Sprintf("Path: `%s`\n", a.fsTool.WorkspaceDir()))
	}

	projects := a.projects.List()
	if msg.Channel == "telegram" {
		if len(projects) == 0 {
			sb.WriteString("\nNo projects registered. Use `/project add <name> <path>`.")
			sendChannelNotification(a.hub, msg.Channel, msg.ChatID, sb.String())
			return
		}
		markup := buildProjectKeyboard(projects, active)
		meta := map[string]interface{}{"reply_markup": markup}
		sb.WriteString("\nTap to switch:")
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, sb.String(), meta)
		return
	}

	for _, p := range projects {
		marker := "  "
		if p.Name == active {
			marker = "✅"
		}
		sb.WriteString(fmt.Sprintf("\n%s `%s` → %s", marker, p.Name, p.Path))
	}
	sb.WriteString("\nSwitch with `/project <name>` or `/project none`.")
	sendChannelNotification(a.hub, msg.Channel, msg.ChatID, sb.String())
}

// sendProjectList lists all registered projects as text.
func (a *AgentLoop) sendProjectList(msg chat.Inbound) {
	projects := a.projects.List()
	active := a.projects.ActiveProject()
	if len(projects) == 0 {
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, "No projects registered. Use `/project add <name> <path>`.")
		return
	}
	var sb strings.Builder
	sb.WriteString("🗂 *Projects*\n")
	for _, p := range projects {
		marker := "  "
		if p.Name == active {
			marker = "✅"
		}
		sb.WriteString(fmt.Sprintf("%s `%s`\n    %s\n", marker, p.Name, p.Path))
	}
	sendChannelNotification(a.hub, msg.Channel, msg.ChatID, sb.String())
}

// handleProjectAdd registers a new project and optionally switches to it.
func (a *AgentLoop) handleProjectAdd(msg chat.Inbound, name, path string) {
	p, err := a.projects.Add(name, path)
	if err != nil {
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("⚠️ %v", err))
		return
	}
	sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("✅ Project `%s` registered → %s\nSwitch with `/project %s`.", p.Name, p.Path, p.Name))
}

// handleProjectRemove unregisters a project. The active project cannot be
// removed — switch away first so the agent never ends up on a dangling path.
func (a *AgentLoop) handleProjectRemove(msg chat.Inbound, name string) {
	if name == a.projects.ActiveProject() {
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("⚠️ `%s` is the active project. Switch first (`/project none`), then remove.", name))
		return
	}
	if err := a.projects.Remove(name); err != nil {
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("⚠️ %v", err))
		return
	}
	sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("🗑️ Project `%s` removed.", name))
}

// handleProjectSwitch switches the active project ("" = profile workspace).
// Refuses to switch while turns are running so tools and prompt context
// never change mid-turn; the caller can /stop or retry after.
func (a *AgentLoop) handleProjectSwitch(msg chat.Inbound, name string) {
	if a.projects == nil {
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, "⚠️ Project registry unavailable.")
		return
	}
	if n := a.busyTurns(); n > 0 {
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("⚠️ %d task(s) still running. `/stop` them or wait — switching projects mid-task is disabled.", n))
		return
	}

	target := a.profileWorkspace
	label := "(profile workspace)"
	if name != "" {
		p, ok := a.projects.Get(name)
		if !ok {
			sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("⚠️ Project `%s` not found. Use `/project list`.", name))
			return
		}
		target = p.Path
		label = fmt.Sprintf("`%s`", p.Name)
	}

	if err := a.applyWorkspace(target); err != nil {
		log.Printf("projects: switch to %q failed: %v", name, err)
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("⚠️ Switch failed: %v", err))
		return
	}
	if err := a.projects.SetActive(name); err != nil {
		log.Printf("projects: persist active %q: %v", name, err)
		sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("⚠️ Switched but failed to persist: %v", err))
		return
	}
	sendChannelNotification(a.hub, msg.Channel, msg.ChatID, fmt.Sprintf("✅ Active project: %s\nPath: `%s`\nSessions are now namespaced to this project.", label, target))
}

// buildProjectKeyboard builds a Telegram inline keyboard for project
// selection, mirroring buildSessionKeyboard. Callback data "prj:<name>" or
// "prj:none" for the profile workspace.
func buildProjectKeyboard(projects []*Project, active string) string {
	type button struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data"`
	}
	var rows [][]button
	rows = append(rows, []button{{
		Text:         "🏠 Profile workspace",
		CallbackData: "prj:none",
	}})
	for _, p := range projects {
		label := p.Name
		if p.Name == active {
			label = "✅ " + p.Name
		}
		rows = append(rows, []button{{
			Text:         label,
			CallbackData: "prj:" + p.Name,
		}})
	}
	type inlineKeyboard struct {
		InlineKeyboard [][]button `json:"inline_keyboard"`
	}
	kb := inlineKeyboard{InlineKeyboard: rows}
	data, err := json.Marshal(kb)
	if err != nil {
		return ""
	}
	return string(data)
}
