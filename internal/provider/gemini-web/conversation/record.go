package conversation

import "time"

// ConversationRecord is the persisted conversation snapshot with metadata.
type ConversationRecord struct {
	Model     string          `json:"model"`
	ClientID  string          `json:"client_id"`
	Metadata  []string        `json:"metadata,omitempty"`
	Messages  []StoredMessage `json:"messages"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}
