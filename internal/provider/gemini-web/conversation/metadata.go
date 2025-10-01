package conversation

import "fmt"

const (
	MetadataMessagesKey = "gemini_web_messages"
	MetadataMatchKey    = "gemini_web_match"
)

// AccountMetaKey builds the key for account-level metadata map.
func AccountMetaKey(accountID, modelName string) string {
	return fmt.Sprintf("account-meta|%s|%s", accountID, modelName)
}
