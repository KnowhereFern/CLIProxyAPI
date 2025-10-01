package geminiwebapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/gemini"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	conversation "github.com/router-for-me/CLIProxyAPI/v6/internal/provider/gemini-web/conversation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	geminiWebDefaultTimeoutSec = 300
)

type GeminiWebState struct {
	cfg         *config.Config
	token       *gemini.GeminiWebTokenStorage
	storagePath string
	authLabel   string

	stableClientID string
	accountID      string

	reqMu  sync.Mutex
	client *GeminiClient

	tokenMu    sync.Mutex
	tokenDirty bool

	convMu    sync.RWMutex
	convStore map[string][]string
	convData  map[string]conversation.ConversationRecord
	convIndex map[string]string

	lastRefresh time.Time

	pendingMatchMu sync.Mutex
	pendingMatch   *conversation.MatchResult
}

type reuseComputation struct {
	metadata []string
	history  []conversation.Message
	overlap  int
}

func NewGeminiWebState(cfg *config.Config, token *gemini.GeminiWebTokenStorage, storagePath, authLabel string) *GeminiWebState {
	state := &GeminiWebState{
		cfg:         cfg,
		token:       token,
		storagePath: storagePath,
		authLabel:   strings.TrimSpace(authLabel),
		convStore:   make(map[string][]string),
		convData:    make(map[string]conversation.ConversationRecord),
		convIndex:   make(map[string]string),
	}
	suffix := conversation.Sha256Hex(token.Secure1PSID)
	if len(suffix) > 16 {
		suffix = suffix[:16]
	}
	state.stableClientID = "gemini-web-" + suffix
	if storagePath != "" {
		base := strings.TrimSuffix(filepath.Base(storagePath), filepath.Ext(storagePath))
		if base != "" {
			state.accountID = base
		} else {
			state.accountID = suffix
		}
	} else {
		state.accountID = suffix
	}
	state.loadConversationCaches()
	return state
}

func (s *GeminiWebState) setPendingMatch(match *conversation.MatchResult) {
	if s == nil {
		return
	}
	s.pendingMatchMu.Lock()
	s.pendingMatch = match
	s.pendingMatchMu.Unlock()
}

func (s *GeminiWebState) consumePendingMatch() *conversation.MatchResult {
	s.pendingMatchMu.Lock()
	defer s.pendingMatchMu.Unlock()
	match := s.pendingMatch
	s.pendingMatch = nil
	return match
}

// SetPendingMatch makes a cached conversation match available for the next request.
func (s *GeminiWebState) SetPendingMatch(match *conversation.MatchResult) {
	s.setPendingMatch(match)
}

// Label returns a stable account label for logging and persistence.
// If a storage file path is known, it uses the file base name (without extension).
// Otherwise, it falls back to the stable client ID (e.g., "gemini-web-<hash>").
func (s *GeminiWebState) Label() string {
	if s == nil {
		return ""
	}
	if s.token != nil {
		if lbl := strings.TrimSpace(s.token.Label); lbl != "" {
			return lbl
		}
	}
	if lbl := strings.TrimSpace(s.authLabel); lbl != "" {
		return lbl
	}
	if s.storagePath != "" {
		base := strings.TrimSuffix(filepath.Base(s.storagePath), filepath.Ext(s.storagePath))
		if base != "" {
			return base
		}
	}
	return s.stableClientID
}

func (s *GeminiWebState) loadConversationCaches() {
	path := s.convPath()
	if path == "" {
		return
	}
	if store, err := conversation.LoadConvStore(path); err == nil {
		s.convStore = store
	}
	if items, index, err := conversation.LoadConvData(path); err == nil {
		s.convData = items
		s.convIndex = index
	}
}

// convPath returns the BoltDB file path used for both account metadata and conversation data.
func (s *GeminiWebState) convPath() string {
	base := s.storagePath
	if base == "" {
		// Use accountID directly as base name; ConvBoltPath will append .bolt.
		base = s.accountID
	}
	return conversation.ConvBoltPath(base)
}

func cloneRoleTextSlice(in []conversation.Message) []conversation.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]conversation.Message, len(in))
	copy(out, in)
	return out
}

func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func longestHistoryOverlap(history, incoming []conversation.Message) int {
	max := len(history)
	if len(incoming) < max {
		max = len(incoming)
	}
	for overlap := max; overlap > 0; overlap-- {
		if conversation.EqualMessages(history[len(history)-overlap:], incoming[:overlap]) {
			return overlap
		}
	}
	return 0
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func storedMessagesToRoleText(stored []conversation.StoredMessage) []conversation.Message {
	if len(stored) == 0 {
		return nil
	}
	converted := make([]conversation.Message, len(stored))
	for i, msg := range stored {
		converted[i] = conversation.Message{Role: msg.Role, Text: msg.Content}
	}
	return converted
}

func (s *GeminiWebState) findConversationByMetadata(model string, metadata []string) ([]conversation.Message, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	s.convMu.RLock()
	defer s.convMu.RUnlock()
	for _, rec := range s.convData {
		if !strings.EqualFold(strings.TrimSpace(rec.Model), strings.TrimSpace(model)) {
			continue
		}
		if !equalStringSlice(rec.Metadata, metadata) {
			continue
		}
		return cloneRoleTextSlice(storedMessagesToRoleText(rec.Messages)), true
	}
	return nil, false
}

func (s *GeminiWebState) GetRequestMutex() *sync.Mutex { return &s.reqMu }

func (s *GeminiWebState) EnsureClient() error {
	if s.client != nil && s.client.Running {
		return nil
	}
	proxyURL := ""
	if s.cfg != nil {
		proxyURL = s.cfg.ProxyURL
	}
	s.client = NewGeminiClient(
		s.token.Secure1PSID,
		s.token.Secure1PSIDTS,
		proxyURL,
	)
	timeout := geminiWebDefaultTimeoutSec
	if err := s.client.Init(float64(timeout), false); err != nil {
		s.client = nil
		return err
	}
	s.lastRefresh = time.Now()
	return nil
}

func (s *GeminiWebState) Refresh(ctx context.Context) error {
	_ = ctx
	proxyURL := ""
	if s.cfg != nil {
		proxyURL = s.cfg.ProxyURL
	}
	s.client = NewGeminiClient(
		s.token.Secure1PSID,
		s.token.Secure1PSIDTS,
		proxyURL,
	)
	timeout := geminiWebDefaultTimeoutSec
	if err := s.client.Init(float64(timeout), false); err != nil {
		return err
	}
	// Attempt rotation proactively to persist new TS sooner
	if newTS, err := s.client.RotateTS(); err == nil && newTS != "" && newTS != s.token.Secure1PSIDTS {
		s.tokenMu.Lock()
		s.token.Secure1PSIDTS = newTS
		s.tokenDirty = true
		if s.client != nil && s.client.Cookies != nil {
			s.client.Cookies["__Secure-1PSIDTS"] = newTS
		}
		s.tokenMu.Unlock()
		// Detailed debug log: provider and account label.
		label := strings.TrimSpace(s.Label())
		if label == "" {
			label = s.accountID
		}
		log.Debugf("gemini web account %s rotated 1PSIDTS: %s", label, MaskToken28(newTS))
	}
	s.lastRefresh = time.Now()
	return nil
}

func (s *GeminiWebState) TokenSnapshot() *gemini.GeminiWebTokenStorage {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	c := *s.token
	return &c
}

type geminiWebPrepared struct {
	handlerType   string
	translatedRaw []byte
	prompt        string
	uploaded      []string
	chat          *ChatSession
	cleaned       []conversation.Message
	underlying    string
	reuse         bool
	tagged        bool
	originalRaw   []byte
}

func (s *GeminiWebState) prepare(ctx context.Context, modelName string, rawJSON []byte, stream bool, original []byte) (*geminiWebPrepared, *interfaces.ErrorMessage) {
	res := &geminiWebPrepared{originalRaw: original}
	res.translatedRaw = bytes.Clone(rawJSON)
	if handler, ok := ctx.Value("handler").(interfaces.APIHandler); ok && handler != nil {
		res.handlerType = handler.HandlerType()
		res.translatedRaw = translator.Request(res.handlerType, constant.GeminiWeb, modelName, res.translatedRaw, stream)
	}
	recordAPIRequest(ctx, s.cfg, res.translatedRaw)

	messages, files, mimes, msgFileIdx, err := ParseMessagesAndFiles(res.translatedRaw)
	if err != nil {
		return nil, &interfaces.ErrorMessage{StatusCode: 400, Error: fmt.Errorf("bad request: %w", err)}
	}
	cleaned := conversation.SanitizeAssistantMessages(messages)
	fullCleaned := cloneRoleTextSlice(cleaned)
	res.underlying = conversation.MapAliasToUnderlying(modelName)
	model, err := ModelFromName(res.underlying)
	if err != nil {
		return nil, &interfaces.ErrorMessage{StatusCode: 400, Error: err}
	}

	var meta []string
	useMsgs := cleaned
	filesSubset := files
	mimesSubset := mimes

	if s.useReusableContext() {
		reusePlan := s.reuseFromPending(res.underlying, cleaned)
		if reusePlan == nil {
			reusePlan = s.findReusableSession(res.underlying, cleaned)
		}
		if reusePlan != nil {
			res.reuse = true
			meta = cloneStringSlice(reusePlan.metadata)
			overlap := reusePlan.overlap
			if overlap > len(cleaned) {
				overlap = len(cleaned)
			} else if overlap < 0 {
				overlap = 0
			}
			delta := cloneRoleTextSlice(cleaned[overlap:])
			if len(reusePlan.history) > 0 {
				fullCleaned = append(cloneRoleTextSlice(reusePlan.history), delta...)
			} else {
				fullCleaned = append(cloneRoleTextSlice(cleaned[:overlap]), delta...)
			}
			useMsgs = delta
			if len(delta) == 0 && len(cleaned) > 0 {
				useMsgs = []conversation.Message{cleaned[len(cleaned)-1]}
			}
			if len(useMsgs) == 1 && len(messages) > 0 && len(msgFileIdx) == len(messages) {
				lastIdx := len(msgFileIdx) - 1
				idxs := msgFileIdx[lastIdx]
				if len(idxs) > 0 {
					filesSubset = make([][]byte, 0, len(idxs))
					mimesSubset = make([]string, 0, len(idxs))
					for _, fi := range idxs {
						if fi >= 0 && fi < len(files) {
							filesSubset = append(filesSubset, files[fi])
							if fi < len(mimes) {
								mimesSubset = append(mimesSubset, mimes[fi])
							} else {
								mimesSubset = append(mimesSubset, "")
							}
						}
					}
				} else {
					filesSubset = nil
					mimesSubset = nil
				}
			} else {
				filesSubset = nil
				mimesSubset = nil
			}
		} else {
			if len(cleaned) >= 2 && strings.EqualFold(cleaned[len(cleaned)-2].Role, "assistant") {
				keyUnderlying := conversation.AccountMetaKey(s.accountID, res.underlying)
				keyAlias := conversation.AccountMetaKey(s.accountID, modelName)
				s.convMu.RLock()
				fallbackMeta := s.convStore[keyUnderlying]
				if len(fallbackMeta) == 0 {
					fallbackMeta = s.convStore[keyAlias]
				}
				s.convMu.RUnlock()
				if len(fallbackMeta) > 0 {
					meta = fallbackMeta
					useMsgs = []conversation.Message{cleaned[len(cleaned)-1]}
					res.reuse = true
					filesSubset = nil
					mimesSubset = nil
				}
			}
		}
	} else {
		keyUnderlying := conversation.AccountMetaKey(s.accountID, res.underlying)
		keyAlias := conversation.AccountMetaKey(s.accountID, modelName)
		s.convMu.RLock()
		if v, ok := s.convStore[keyUnderlying]; ok && len(v) > 0 {
			meta = v
		} else {
			meta = s.convStore[keyAlias]
		}
		s.convMu.RUnlock()
	}

	res.cleaned = fullCleaned

	res.tagged = conversation.NeedRoleTags(useMsgs)
	if res.reuse && len(useMsgs) == 1 {
		res.tagged = false
	}

	enableXML := s.cfg != nil && s.cfg.GeminiWeb.CodeMode
	useMsgs = AppendXMLWrapHintIfNeeded(useMsgs, !enableXML)

	res.prompt = conversation.BuildPrompt(useMsgs, res.tagged, res.tagged)
	if strings.TrimSpace(res.prompt) == "" {
		return nil, &interfaces.ErrorMessage{StatusCode: 400, Error: errors.New("bad request: empty prompt after filtering system/thought content")}
	}

	uploaded, upErr := MaterializeInlineFiles(filesSubset, mimesSubset)
	if upErr != nil {
		return nil, upErr
	}
	res.uploaded = uploaded

	if err = s.EnsureClient(); err != nil {
		return nil, &interfaces.ErrorMessage{StatusCode: 500, Error: err}
	}
	chat := s.client.StartChat(model, s.getConfiguredGem(), meta)
	chat.SetRequestedModel(modelName)
	res.chat = chat

	return res, nil
}

func (s *GeminiWebState) Send(ctx context.Context, modelName string, reqPayload []byte, opts cliproxyexecutor.Options) ([]byte, *interfaces.ErrorMessage, *geminiWebPrepared) {
	prep, errMsg := s.prepare(ctx, modelName, reqPayload, opts.Stream, opts.OriginalRequest)
	if errMsg != nil {
		return nil, errMsg, nil
	}
	defer CleanupFiles(prep.uploaded)

	output, err := SendWithSplit(prep.chat, prep.prompt, prep.uploaded, s.cfg)
	if err != nil {
		return nil, s.wrapSendError(err), nil
	}

	// Hook: For gemini-2.5-flash-image-preview, if the API returns only images without any text,
	// inject a small textual summary so that conversation persistence has non-empty assistant text.
	// This helps conversation recovery (conv store) to match sessions reliably.
	if strings.EqualFold(modelName, "gemini-2.5-flash-image-preview") {
		if len(output.Candidates) > 0 {
			c := output.Candidates[output.Chosen]
			hasNoText := strings.TrimSpace(c.Text) == ""
			hasImages := len(c.GeneratedImages) > 0 || len(c.WebImages) > 0
			if hasNoText && hasImages {
				// Build a stable, concise fallback text. Avoid dynamic details to keep hashes stable.
				// Prefer a deterministic phrase with count to aid users while keeping consistency.
				fallback := "Done"
				// Mutate the chosen candidate's text so both response conversion and
				// conversation persistence observe the same fallback.
				output.Candidates[output.Chosen].Text = fallback
			}
		}
	}

	gemBytes, err := ConvertOutputToGemini(&output, modelName, prep.prompt)
	if err != nil {
		return nil, &interfaces.ErrorMessage{StatusCode: 500, Error: err}, nil
	}

	s.addAPIResponseData(ctx, gemBytes)
	s.persistConversation(modelName, prep, &output)
	return gemBytes, nil, prep
}

func (s *GeminiWebState) wrapSendError(genErr error) *interfaces.ErrorMessage {
	status := 500
	var usage *UsageLimitExceeded
	var blocked *TemporarilyBlocked
	var invalid *ModelInvalid
	var valueErr *ValueError
	var timeout *TimeoutError
	switch {
	case errors.As(genErr, &usage):
		status = 429
	case errors.As(genErr, &blocked):
		status = 429
	case errors.As(genErr, &invalid):
		status = 400
	case errors.As(genErr, &valueErr):
		status = 400
	case errors.As(genErr, &timeout):
		status = 504
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: genErr}
}

func (s *GeminiWebState) persistConversation(modelName string, prep *geminiWebPrepared, output *ModelOutput) {
	if output == nil || prep == nil || prep.chat == nil {
		return
	}
	metadata := prep.chat.Metadata()
	if len(metadata) > 0 {
		keyUnderlying := conversation.AccountMetaKey(s.accountID, prep.underlying)
		keyAlias := conversation.AccountMetaKey(s.accountID, modelName)
		s.convMu.Lock()
		s.convStore[keyUnderlying] = metadata
		s.convStore[keyAlias] = metadata
		storeSnapshot := make(map[string][]string, len(s.convStore))
		for k, v := range s.convStore {
			if v == nil {
				continue
			}
			cp := make([]string, len(v))
			copy(cp, v)
			storeSnapshot[k] = cp
		}
		s.convMu.Unlock()
		_ = conversation.SaveConvStore(s.convPath(), storeSnapshot)
	}

	if !s.useReusableContext() {
		return
	}
	rec, ok := buildConversationRecord(prep.underlying, s.stableClientID, prep.cleaned, output, metadata)
	if !ok {
		return
	}
	label := strings.TrimSpace(s.Label())
	if label == "" {
		label = s.accountID
	}
	conversationMsgs := conversation.StoredToMessages(rec.Messages)
	if err := conversation.StoreConversation(label, prep.underlying, conversationMsgs, metadata); err != nil {
		log.Debugf("gemini web: failed to persist global conversation index: %v", err)
	}
	stableHash := conversation.HashConversationForAccount(rec.ClientID, prep.underlying, rec.Messages)
	accountHash := conversation.HashConversationForAccount(s.accountID, prep.underlying, rec.Messages)

	suffixSeen := make(map[string]struct{})
	suffixSeen["hash:"+stableHash] = struct{}{}
	if accountHash != stableHash {
		suffixSeen["hash:"+accountHash] = struct{}{}
	}

	s.convMu.Lock()
	s.convData[stableHash] = rec
	s.convIndex["hash:"+stableHash] = stableHash
	if accountHash != stableHash {
		s.convIndex["hash:"+accountHash] = stableHash
	}

	sanitizedHistory := conversation.SanitizeAssistantMessages(conversation.StoredToMessages(rec.Messages))
	for start := 1; start < len(sanitizedHistory); start++ {
		segment := sanitizedHistory[start:]
		if len(segment) < 2 {
			continue
		}
		tailRole := strings.ToLower(strings.TrimSpace(segment[len(segment)-1].Role))
		if tailRole != "assistant" && tailRole != "system" {
			continue
		}
		storedSegment := conversation.ToStoredMessages(segment)
		segmentStableHash := conversation.HashConversationForAccount(rec.ClientID, prep.underlying, storedSegment)
		keyStable := "hash:" + segmentStableHash
		if _, exists := suffixSeen[keyStable]; !exists {
			s.convIndex[keyStable] = stableHash
			suffixSeen[keyStable] = struct{}{}
		}
		segmentAccountHash := conversation.HashConversationForAccount(s.accountID, prep.underlying, storedSegment)
		if segmentAccountHash != segmentStableHash {
			keyAccount := "hash:" + segmentAccountHash
			if _, exists := suffixSeen[keyAccount]; !exists {
				s.convIndex[keyAccount] = stableHash
				suffixSeen[keyAccount] = struct{}{}
			}
		}
	}
	dataSnapshot := make(map[string]conversation.ConversationRecord, len(s.convData))
	for k, v := range s.convData {
		dataSnapshot[k] = v
	}
	indexSnapshot := make(map[string]string, len(s.convIndex))
	for k, v := range s.convIndex {
		indexSnapshot[k] = v
	}
	s.convMu.Unlock()
	_ = conversation.SaveConvData(s.convPath(), dataSnapshot, indexSnapshot)
}

func (s *GeminiWebState) addAPIResponseData(ctx context.Context, line []byte) {
	appendAPIResponseChunk(ctx, s.cfg, line)
}

func (s *GeminiWebState) ConvertToTarget(ctx context.Context, modelName string, prep *geminiWebPrepared, gemBytes []byte) []byte {
	if prep == nil || prep.handlerType == "" {
		return gemBytes
	}
	if !translator.NeedConvert(prep.handlerType, constant.GeminiWeb) {
		return gemBytes
	}
	var param any
	out := translator.ResponseNonStream(prep.handlerType, constant.GeminiWeb, ctx, modelName, prep.originalRaw, prep.translatedRaw, gemBytes, &param)
	if prep.handlerType == constant.OpenAI && out != "" {
		newID := fmt.Sprintf("chatcmpl-%x", time.Now().UnixNano())
		if v := gjson.Parse(out).Get("id"); v.Exists() {
			out, _ = sjson.Set(out, "id", newID)
		}
	}
	return []byte(out)
}

func (s *GeminiWebState) ConvertStream(ctx context.Context, modelName string, prep *geminiWebPrepared, gemBytes []byte) []string {
	if prep == nil || prep.handlerType == "" {
		return []string{string(gemBytes)}
	}
	if !translator.NeedConvert(prep.handlerType, constant.GeminiWeb) {
		return []string{string(gemBytes)}
	}
	var param any
	return translator.Response(prep.handlerType, constant.GeminiWeb, ctx, modelName, prep.originalRaw, prep.translatedRaw, gemBytes, &param)
}

func (s *GeminiWebState) DoneStream(ctx context.Context, modelName string, prep *geminiWebPrepared) []string {
	if prep == nil || prep.handlerType == "" {
		return nil
	}
	if !translator.NeedConvert(prep.handlerType, constant.GeminiWeb) {
		return nil
	}
	var param any
	return translator.Response(prep.handlerType, constant.GeminiWeb, ctx, modelName, prep.originalRaw, prep.translatedRaw, []byte("[DONE]"), &param)
}

func (s *GeminiWebState) useReusableContext() bool {
	if s.cfg == nil {
		return true
	}
	return s.cfg.GeminiWeb.Context
}

func (s *GeminiWebState) reuseFromPending(modelName string, msgs []conversation.Message) *reuseComputation {
	match := s.consumePendingMatch()
	if match == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(match.Model), strings.TrimSpace(modelName)) {
		return nil
	}
	metadata := cloneStringSlice(match.Record.Metadata)
	if len(metadata) == 0 {
		return nil
	}
	history, ok := s.findConversationByMetadata(modelName, metadata)
	if !ok {
		return nil
	}
	overlap := longestHistoryOverlap(history, msgs)
	return &reuseComputation{metadata: metadata, history: history, overlap: overlap}
}

func (s *GeminiWebState) findReusableSession(modelName string, msgs []conversation.Message) *reuseComputation {
	s.convMu.RLock()
	items := s.convData
	index := s.convIndex
	s.convMu.RUnlock()
	rec, metadata, overlap, ok := conversation.FindReusableSessionIn(items, index, s.stableClientID, s.accountID, modelName, msgs)
	if !ok {
		return nil
	}
	history := cloneRoleTextSlice(storedMessagesToRoleText(rec.Messages))
	if len(history) == 0 {
		return nil
	}
	// Ensure overlap reflects the actual history alignment.
	if computed := longestHistoryOverlap(history, msgs); computed > 0 {
		overlap = computed
	}
	return &reuseComputation{metadata: cloneStringSlice(metadata), history: history, overlap: overlap}
}

func (s *GeminiWebState) getConfiguredGem() *Gem {
	if s.cfg != nil && s.cfg.GeminiWeb.CodeMode {
		return &Gem{ID: "coding-partner", Name: "Coding partner", Predefined: true}
	}
	return nil
}

// recordAPIRequest stores the upstream request payload in Gin context for request logging.
func recordAPIRequest(ctx context.Context, cfg *config.Config, payload []byte) {
	if cfg == nil || !cfg.RequestLog || len(payload) == 0 {
		return
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
		ginCtx.Set("API_REQUEST", bytes.Clone(payload))
	}
}

// appendAPIResponseChunk appends an upstream response chunk to Gin context for request logging.
func appendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	if cfg == nil || !cfg.RequestLog {
		return
	}
	data := bytes.TrimSpace(bytes.Clone(chunk))
	if len(data) == 0 {
		return
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
		if existing, exists := ginCtx.Get("API_RESPONSE"); exists {
			if prev, okBytes := existing.([]byte); okBytes {
				prev = append(prev, data...)
				prev = append(prev, []byte("\n\n")...)
				ginCtx.Set("API_RESPONSE", prev)
				return
			}
		}
		ginCtx.Set("API_RESPONSE", data)
	}
}

// buildConversationRecord constructs a ConversationRecord from history and the latest output.
// Returns false when output is empty or has no candidates.
func buildConversationRecord(model, clientID string, history []conversation.Message, output *ModelOutput, metadata []string) (conversation.ConversationRecord, bool) {
	if output == nil || len(output.Candidates) == 0 {
		return conversation.ConversationRecord{}, false
	}
	text := ""
	if t := output.Candidates[0].Text; t != "" {
		text = conversation.RemoveThinkTags(t)
	}
	final := append([]conversation.Message{}, history...)
	final = append(final, conversation.Message{Role: "assistant", Text: text})
	rec := conversation.ConversationRecord{
		Model:     model,
		ClientID:  clientID,
		Metadata:  metadata,
		Messages:  conversation.ToStoredMessages(final),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	return rec, true
}
