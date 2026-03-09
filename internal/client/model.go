// Package client implements the matechat TUI using bubbletea.
package client

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"matechat/internal/peer"
	"matechat/internal/proto"
	"matechat/internal/store"
)

// Message types for the tea loop
type incomingMsg struct {
	msg proto.ChatMsg
}

type peerJoinMsg struct {
	name string
}

type peerLeaveMsg struct {
	name string
}

type historyLoadedMsg struct {
	messages []displayMsg
}

type displayMsg struct {
	From string
	Body string
}

// Model is the bubbletea model for the matechat TUI.
type Model struct {
	manager  *peer.Manager
	store    *store.Store
	messages []displayMsg
	peers    []string
	version  string

	input    textinput.Model
	viewport viewport.Model

	width  int
	height int
	ready  bool

	msgChan chan proto.ChatMsg
	joinCh  chan string
	leaveCh chan string
}

// New creates a new Model with pre-created channels for manager communication.
func New(manager *peer.Manager, st *store.Store,
	msgCh chan proto.ChatMsg, joinCh, leaveCh chan string, version string) Model {
	ti := textinput.New()
	ti.Placeholder = "type a message..."
	ti.Focus()

	return Model{
		manager: manager,
		store:   st,
		input:   ti,
		msgChan: msgCh,
		joinCh:  joinCh,
		leaveCh: leaveCh,
		version: version,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		m.waitForMessage(),
		m.waitForPeerJoin(),
		m.waitForPeerLeave(),
		m.loadHistory(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		headerHeight := 2
		inputHeight := 1
		vpHeight := m.height - headerHeight - inputHeight - 2

		if !m.ready {
			m.viewport = viewport.New(m.width, vpHeight)
			m.viewport.SetContent(m.renderMessages())
			m.ready = true
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = vpHeight
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.manager.Shutdown()
			return m, tea.Quit
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text != "" {
				m.input.Reset()
				m.messages = append(m.messages, displayMsg{
					From: m.manager.SelfName(),
					Body: text,
				})
				if m.ready {
					m.viewport.SetContent(m.renderMessages())
					m.viewport.GotoBottom()
				}
				go m.manager.Broadcast(text)
			}
			return m, nil
		}

	case incomingMsg:
		m.messages = append(m.messages, displayMsg{
			From: msg.msg.From,
			Body: msg.msg.Body,
		})
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
		cmds = append(cmds, m.waitForMessage())

	case peerJoinMsg:
		m.peers = m.manager.OnlinePeers()
		sort.Strings(m.peers)
		cmds = append(cmds, m.waitForPeerJoin())

	case peerLeaveMsg:
		m.peers = m.manager.OnlinePeers()
		sort.Strings(m.peers)
		cmds = append(cmds, m.waitForPeerLeave())

	case historyLoadedMsg:
		m.messages = msg.messages
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	if m.ready {
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if !m.ready {
		return "connecting..."
	}

	header := m.renderHeader()
	body := m.viewport.View()
	input := m.input.View()

	return fmt.Sprintf("%s\n%s\n%s", header, body, input)
}

func (m Model) renderHeader() string {
	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("229")).
		Render("matechat " + m.version)

	var peers []string
	for _, name := range m.peers {
		style := lipgloss.NewStyle().
			Foreground(nameColor(name))
		peers = append(peers, style.Render("● "+name))
	}

	selfStyle := lipgloss.NewStyle().
		Foreground(nameColor(m.manager.SelfName())).
		Bold(true)
	self := selfStyle.Render("● " + m.manager.SelfName())

	peerList := self
	if len(peers) > 0 {
		peerList = self + "  " + strings.Join(peers, "  ")
	}

	return title + "  " + peerList
}

func (m Model) renderMessages() string {
	if len(m.messages) == 0 {
		return lipgloss.NewStyle().
			Faint(true).
			Render("  no messages yet — say something!")
	}

	var sb strings.Builder
	for _, msg := range m.messages {
		nameStyle := lipgloss.NewStyle().
			Foreground(nameColor(msg.From)).
			Bold(true)
		sb.WriteString(fmt.Sprintf("  %s %s\n", nameStyle.Render("["+msg.From+"]"), msg.Body))
	}
	return sb.String()
}

func (m Model) waitForMessage() tea.Cmd {
	return func() tea.Msg {
		msg := <-m.msgChan
		return incomingMsg{msg: msg}
	}
}

func (m Model) waitForPeerJoin() tea.Cmd {
	return func() tea.Msg {
		name := <-m.joinCh
		return peerJoinMsg{name: name}
	}
}

func (m Model) waitForPeerLeave() tea.Cmd {
	return func() tea.Msg {
		name := <-m.leaveCh
		return peerLeaveMsg{name: name}
	}
}

func (m Model) loadHistory() tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.store.RecentMessages(100)
		if err != nil {
			return historyLoadedMsg{}
		}
		display := make([]displayMsg, len(msgs))
		for i, msg := range msgs {
			display[i] = displayMsg{From: msg.From, Body: msg.Body}
		}
		return historyLoadedMsg{messages: display}
	}
}

func nameColor(name string) lipgloss.Color {
	colors := []string{
		"#E06B6B", // red     (  0°)
		"#E0C36C", // gold    ( 45°)
		"#A6E06C", // lime    ( 90°)
		"#6CE089", // green   (135°)
		"#6CE0E0", // cyan    (180°)
		"#6C89E0", // blue    (225°)
		"#A66CE0", // purple  (270°)
		"#E06CC3", // pink    (315°)
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return lipgloss.Color(colors[int(h.Sum32())%len(colors)])
}
