package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"github.com/matrix-org/gomatrix"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

var bullets = []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣"}

// config is the configuration structure.
type config struct {
	Matrix struct {
		AccessToken string `yaml:"access_token"`
		UserID      string `yaml:"user_id"`
		HSURL       string `yaml:"hs_url"`
		SkipFilter  bool   `yaml:"skip_filter"`
	}
}

// reaction is the content of m.reaction events.
type reaction struct {
	RelatesTo struct {
		RelType string `json:"rel_type"`
		Key     string `json:"key"`
		EventID string `json:"event_id"`
	} `json:"m.relates_to"`
}

// handler defines the handlers to call when processing incoming Matrix events, along with some util functions.
type handler struct {
	Client *gomatrix.Client
}

func newHandler(homeserverURL, userID, accessToken string) (h *handler, err error) {
	h = new(handler)

	h.Client, err = gomatrix.NewClient(homeserverURL, userID, accessToken)
	if err != nil {
		return
	}

	return
}

// setupFilter configures the /sync filter on the Matrix homeserver.
// Returns the filter ID.
func (h *handler) setupFilter() string {
	filter := gomatrix.Filter{
		Room: gomatrix.RoomFilter{
			Timeline: gomatrix.FilterPart{
				Types: []string{
					"m.room.message",
					"m.room.member",
				},
			},
		},
		EventFields: []string{
			"type",               // Needed to register the handlers.
			"event_id",           // Needed for logging.
			"room_id",            // Needed to manage the rooms we're in.
			"state_key",          // Needed for the syncer to manage the room's state.
			"sender",             // Needed to mention the poll's author.
			"content.body",       // Needed to process messages.
			"content.membership", // Needed to process invites.
		},
	}

	filterJSON, err := json.Marshal(filter)
	if err != nil {
		panic(err)
	}

	resp, err := h.Client.CreateFilter(filterJSON)
	if err != nil {
		panic(err)
	}

	return resp.FilterID
}

// handleMessage handles incoming m.room.messages events.
func (h *handler) handleMessage(event *gomatrix.Event) {
	logger := logrus.WithFields(logrus.Fields{
		"event_id": event.ID,
		"room_id":  event.RoomID,
	})

	body := event.Content["body"].(string)

	// Extract the question and choices from the event's content's body.
	question, choices, errMsg := h.parseMessage(body)
	if len(errMsg) > 0 {
		// Send the error message as a notice to the room.
		if _, err := h.Client.SendNotice(event.RoomID, errMsg); err != nil {
			logger.WithField("errMsg", errMsg).Errorf("Failed to send error message: %s", err.Error())
		}
		return
	}

	if len(question) == 0 {
		// This message is not a poll.
		return
	}

	// Generate the HTML string to send as a notice.
	notice := h.generateNoticeHTML(event.Sender, question, choices)

	// Send the poll to the room.
	res, err := h.Client.SendMessageEvent(
		event.RoomID, "m.room.message", gomatrix.GetHTMLMessage("m.notice", notice),
	)
	if err != nil {
		logger.Errorf("Couldn't send poll to room: %s", err.Error())
		return
	}

	// Add reactions to the poll message.
	for index, _ := range choices {
		content := reaction{}
		content.RelatesTo.RelType = "m.annotation"
		content.RelatesTo.Key = bullets[index]
		content.RelatesTo.EventID = res.EventID

		if _, err := h.Client.SendMessageEvent(event.RoomID, "m.reaction", &content); err != nil {
			logger.WithField("poll_event_id", res.EventID).Errorf(
				"Couldn't send reaction to poll: %s", err.Error(),
			)
			return
		}
	}

	// Redact the original message.
	if _, err := h.Client.RedactEvent(event.RoomID, event.ID, &gomatrix.ReqRedact{Reason: "poll"}); err != nil {
		logger.WithField("poll_event_id", res.EventID).Error(
			"Couldn't redact message. Maybe set to power level higher?",
		)
		return
	}

}

// parseMessage extracts the question and choices from the given string.
// Returns with the question and choices, and an error string to send to the room if the message isn't formatted
// correctly.
func (h *handler) parseMessage(body string) (question string, choices []string, errMsg string) {
	r := strings.NewReader(body)

	scanner := bufio.NewScanner(r)

	choices = make([]string, 0)
	firstLine := true
	// Read the message line by line.
	for scanner.Scan() {
		line := scanner.Text()

		if firstLine {
			// If the line starts with "!poll", it's the question, otherwise it's not a message for us.
			if strings.HasPrefix(line, "!poll ") {
				question = strings.Replace(line, "!poll ", "", 1)
			} else {
				// Don't send an error message since the message isn't for us.
				return
			}
		} else {
			// handle empty lines
			if len(line) < 3 {
				continue
			}

			choices = append(choices, line)
		}

		firstLine = false
	}

	return
}

// generateNoticeHTML generates the HTML text to send as a notice to the room.
func (h *handler) generateNoticeHTML(userID string, question string, choices []string) string {
	displayName := userID

	// Try to retrieve the sender's display name from the homeserver. If we can't, use the user ID instead.
	res, err := h.Client.GetDisplayName(userID)
	if err == nil {
		displayName = res.DisplayName
	}

	format := `
	<b>New poll from <a href="https://matrix.to/#/%s">%s</a>!</b><br><br>
	Question: <i><u>%s</u></i><br>
	Cast your vote by clicking on the reactions under this message (if available).<br><br>
	Choices:<br>
	`

	notice := fmt.Sprintf(format, userID, displayName, question)

	for index, value := range choices {
		notice += fmt.Sprintf("%s: %s<br>", bullets[index], value)
	}

	return notice
}

// handleMembership handles m.room.member events, and autojoins rooms it's invited in.
func (h *handler) handleMembership(event *gomatrix.Event) {
	var joinDelay time.Duration = 1

	if membership, ok := event.Content["membership"]; !ok || membership != "invite" {
		return
	}

	logrus.Infof("Trying to join room %s I was invited to", event.RoomID)

	time.Sleep(joinDelay * time.Second)
	_, err := h.Client.JoinRoom(event.RoomID, "", struct{}{})
	if err != nil {
		logrus.Errorf("Failed to join room %s: %s", event.RoomID, err.Error())
	}

	logrus.Infof("Successfully joined room %s", event.RoomID)
}

func main() {
	var cfg config

	configFile := flag.String("config", "config.yaml", "Path to the configuration file")

	flag.Parse()

	configBytes, err := ioutil.ReadFile(*configFile)
	if err != nil {
		panic(errors.Wrap(err, "Couldn't open the configuration file"))
	}

	if err := yaml.Unmarshal(configBytes, &cfg); err != nil {
		panic(errors.Wrap(err, "Couldn't read the configuration file"))
	}

	h, err := newHandler(cfg.Matrix.HSURL, cfg.Matrix.UserID, cfg.Matrix.AccessToken)
	if err != nil {
		panic(errors.Wrap(err, "Couldn't initialise the Matrix client"))
	}

	if !cfg.Matrix.SkipFilter {
		filterID := h.setupFilter()

		h.Client.Store.SaveFilterID(cfg.Matrix.UserID, filterID)
	}

	syncer := h.Client.Syncer.(*gomatrix.DefaultSyncer)

	syncer.OnEventType("m.room.message", h.handleMessage)
	syncer.OnEventType("m.room.member", h.handleMembership)

	logrus.Info("Syncing...")
	if err := h.Client.Sync(); err != nil {
		panic(errors.Wrap(err, "Sync returned with an error"))
	}
}
