package conversation

import "strings"

// PrefixHash represents a hash candidate for a specific prefix length.
type PrefixHash struct {
	Hash      string
	PrefixLen int
}

// LookupResult represents a successful lookup result with metadata.
type LookupResult struct {
	Key        string   // The key found in the store
	Metadata   []string // Associated metadata
	OverlapLen int      // Number of overlapping messages (for reusable sessions)
}

// MetadataGetter is an interface for retrieving metadata from a store.
type MetadataGetter interface {
	GetMetadata(key string) ([]string, bool)
}

// BuildLookupHashes generates hash candidates ordered from longest to shortest prefix.
func BuildLookupHashes(model string, msgs []Message) []PrefixHash {
	if len(msgs) < 2 {
		return nil
	}
	model = NormalizeModel(model)
	sanitized := SanitizeAssistantMessages(msgs)
	result := make([]PrefixHash, 0, len(sanitized))
	for end := len(sanitized); end >= 2; end-- {
		tailRole := strings.ToLower(strings.TrimSpace(sanitized[end-1].Role))
		if tailRole != "assistant" && tailRole != "system" {
			continue
		}
		prefix := sanitized[:end]
		hash := HashConversationGlobal(model, ToStoredMessages(prefix))
		result = append(result, PrefixHash{Hash: hash, PrefixLen: end})
	}
	return result
}

// BuildStorageHashes returns hashes representing the full conversation snapshot.
func BuildStorageHashes(model string, msgs []Message) []PrefixHash {
	if len(msgs) == 0 {
		return nil
	}
	model = NormalizeModel(model)
	sanitized := SanitizeAssistantMessages(msgs)
	if len(sanitized) == 0 {
		return nil
	}
	result := make([]PrefixHash, 0, len(sanitized))
	seen := make(map[string]struct{}, len(sanitized))
	for start := 0; start < len(sanitized); start++ {
		segment := sanitized[start:]
		if len(segment) < 2 {
			continue
		}
		tailRole := strings.ToLower(strings.TrimSpace(segment[len(segment)-1].Role))
		if tailRole != "assistant" && tailRole != "system" {
			continue
		}
		hash := HashConversationGlobal(model, ToStoredMessages(segment))
		if _, exists := seen[hash]; exists {
			continue
		}
		seen[hash] = struct{}{}
		result = append(result, PrefixHash{Hash: hash, PrefixLen: len(segment)})
	}
	if len(result) == 0 {
		hash := HashConversationGlobal(model, ToStoredMessages(sanitized))
		return []PrefixHash{{Hash: hash, PrefixLen: len(sanitized)}}
	}
	return result
}

// FindByMessageHash looks up a conversation by hashed message list.
// It attempts both the stable client ID and a legacy email-based ID.
// Returns the key found in items or index, or empty string if not found.
func FindByMessageHash(items map[string]bool, index map[string]string, stableClientID, email, model string, msgs []Message) string {
	stored := ToStoredMessages(msgs)
	stableHash := HashConversationForAccount(stableClientID, model, stored)
	fallbackHash := HashConversationForAccount(email, model, stored)

	// Try stable hash via index indirection first
	if key, ok := index["hash:"+stableHash]; ok {
		if items[key] {
			return key
		}
	}
	if items[stableHash] {
		return stableHash
	}
	// Fallback to legacy hash (email-based)
	if key, ok := index["hash:"+fallbackHash]; ok {
		if items[key] {
			return key
		}
	}
	if items[fallbackHash] {
		return fallbackHash
	}
	return ""
}

// FindConversationKey tries exact then sanitized assistant messages.
// Returns the key found, or empty string if not found.
func FindConversationKey(items map[string]bool, index map[string]string, stableClientID, email, model string, msgs []Message) string {
	if len(msgs) == 0 {
		return ""
	}
	if key := FindByMessageHash(items, index, stableClientID, email, model, msgs); key != "" {
		return key
	}
	if key := FindByMessageHash(items, index, stableClientID, email, model, SanitizeAssistantMessages(msgs)); key != "" {
		return key
	}
	return ""
}

// FindReusableSessionKey returns a key for a reusable session and the overlap length.
// It searches for the longest matching prefix ending with assistant/system message.
func FindReusableSessionKey(items map[string]bool, index map[string]string, stableClientID, email, model string, msgs []Message) (key string, overlapLen int) {
	if len(msgs) < 2 {
		return "", 0
	}
	searchEnd := len(msgs)
	for searchEnd >= 2 {
		sub := msgs[:searchEnd]
		tail := sub[len(sub)-1]
		if strings.EqualFold(tail.Role, "assistant") || strings.EqualFold(tail.Role, "system") {
			if foundKey := FindConversationKey(items, index, stableClientID, email, model, sub); foundKey != "" {
				return foundKey, searchEnd
			}
		}
		searchEnd--
	}
	return "", 0
}

// FindByMessageListIn looks up a conversation record by hashed message list.
// It attempts both the stable client ID and a legacy email-based ID.
// Returns the record and true if found.
func FindByMessageListIn(items map[string]ConversationRecord, index map[string]string, stableClientID, email, model string, msgs []Message) (ConversationRecord, bool) {
	// Build a key set for generic lookup
	keySet := make(map[string]bool, len(items))
	for k := range items {
		keySet[k] = true
	}
	key := FindByMessageHash(keySet, index, stableClientID, email, model, msgs)
	if key == "" {
		return ConversationRecord{}, false
	}
	rec, ok := items[key]
	return rec, ok
}

// FindConversationIn tries exact then sanitized assistant messages.
func FindConversationIn(items map[string]ConversationRecord, index map[string]string, stableClientID, email, model string, msgs []Message) (ConversationRecord, bool) {
	// Build a key set for generic lookup
	keySet := make(map[string]bool, len(items))
	for k := range items {
		keySet[k] = true
	}
	key := FindConversationKey(keySet, index, stableClientID, email, model, msgs)
	if key == "" {
		return ConversationRecord{}, false
	}
	rec, ok := items[key]
	return rec, ok
}

// FindReusableSessionIn returns reusable metadata and the remaining message suffix.
func FindReusableSessionIn(items map[string]ConversationRecord, index map[string]string, stableClientID, email, model string, msgs []Message) (ConversationRecord, []string, int, bool) {
	// Build a key set for generic lookup
	keySet := make(map[string]bool, len(items))
	for k := range items {
		keySet[k] = true
	}
	key, overlapLen := FindReusableSessionKey(keySet, index, stableClientID, email, model, msgs)
	if key == "" {
		return ConversationRecord{}, nil, 0, false
	}
	rec, ok := items[key]
	if !ok {
		return ConversationRecord{}, nil, 0, false
	}
	return rec, rec.Metadata, overlapLen, true
}
