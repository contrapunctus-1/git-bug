package core

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MichaelMure/git-bug/entity"
	"github.com/MichaelMure/git-bug/repository"
)

const (
	tokenConfigKeyPrefix = "git-bug.token"
	tokenValueKey        = "value"
	tokenTargetKey       = "target"
	tokenCreateTimeKey   = "createtime"
)

var ErrTokenNotExist = errors.New("token doesn't exist")

func NewErrMultipleMatchToken(matching []entity.Id) *entity.ErrMultipleMatch {
	return entity.NewErrMultipleMatch("token", matching)
}

// Token holds an API access token data
type Token struct {
	Value      string
	Target     string
	CreateTime time.Time
}

// NewToken instantiate a new token
func NewToken(value, target string) *Token {
	return &Token{
		Value:      value,
		Target:     target,
		CreateTime: time.Now(),
	}
}

func (t *Token) ID() entity.Id {
	sum := sha256.Sum256([]byte(t.Target + t.Value))
	return entity.Id(fmt.Sprintf("%x", sum))
}

// Validate ensure token important fields are valid
func (t *Token) Validate() error {
	if t.Value == "" {
		return fmt.Errorf("missing value")
	}
	if t.Target == "" {
		return fmt.Errorf("missing target")
	}
	if t.CreateTime.IsZero() || t.CreateTime.Equal(time.Time{}) {
		return fmt.Errorf("missing creation time")
	}
	if !TargetExist(t.Target) {
		return fmt.Errorf("unknown target")
	}
	return nil
}

// LoadToken loads a token from the repo config
func LoadToken(repo repository.RepoCommon, id entity.Id) (*Token, error) {
	keyPrefix := fmt.Sprintf("git-bug.token.%s.", id)

	// read token config pairs
	rawconfigs, err := repo.GlobalConfig().ReadAll(keyPrefix)
	if err != nil {
		// Not exactly right due to the limitation of ReadAll()
		return nil, ErrTokenNotExist
	}

	// trim key prefix
	configs := make(map[string]string)
	for key, value := range rawconfigs {
		newKey := strings.TrimPrefix(key, keyPrefix)
		configs[newKey] = value
	}

	token := &Token{}

	token.Value = configs[tokenValueKey]
	token.Target = configs[tokenTargetKey]
	if createTime, ok := configs[tokenCreateTimeKey]; ok {
		if t, err := repository.ParseTimestamp(createTime); err == nil {
			token.CreateTime = t
		}
	}

	return token, nil
}

// LoadTokenPrefix load a token from the repo config with a prefix
func LoadTokenPrefix(repo repository.RepoCommon, prefix string) (*Token, error) {
	tokens, err := ListTokens(repo)
	if err != nil {
		return nil, err
	}

	// preallocate but empty
	matching := make([]entity.Id, 0, 5)

	for _, id := range tokens {
		if id.HasPrefix(prefix) {
			matching = append(matching, id)
		}
	}

	if len(matching) > 1 {
		return nil, NewErrMultipleMatchToken(matching)
	}

	if len(matching) == 0 {
		return nil, ErrTokenNotExist
	}

	return LoadToken(repo, matching[0])
}

// ListTokens list all existing token ids
func ListTokens(repo repository.RepoCommon) ([]entity.Id, error) {
	configs, err := repo.GlobalConfig().ReadAll(tokenConfigKeyPrefix + ".")
	if err != nil {
		return nil, err
	}

	re, err := regexp.Compile(tokenConfigKeyPrefix + `.([^.]+)`)
	if err != nil {
		panic(err)
	}

	set := make(map[string]interface{})

	for key := range configs {
		res := re.FindStringSubmatch(key)

		if res == nil {
			continue
		}

		set[res[1]] = nil
	}

	result := make([]entity.Id, 0, len(set))
	for key := range set {
		result = append(result, entity.Id(key))
	}

	sort.Sort(entity.Alphabetical(result))

	return result, nil
}

// ListTokensWithTarget list all token ids associated with the target
func ListTokensWithTarget(repo repository.RepoCommon, target string) ([]entity.Id, error) {
	var ids []entity.Id
	tokensIds, err := ListTokens(repo)
	if err != nil {
		return nil, err
	}

	for _, tokenId := range tokensIds {
		token, err := LoadToken(repo, tokenId)
		if err != nil {
			return nil, err
		}

		if token.Target == target {
			ids = append(ids, tokenId)
		}
	}
	return ids, nil
}

// LoadTokens load all existing tokens
func LoadTokens(repo repository.RepoCommon) ([]*Token, error) {
	tokensIds, err := ListTokens(repo)
	if err != nil {
		return nil, err
	}

	var tokens []*Token
	for _, id := range tokensIds {
		token, err := LoadToken(repo, id)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, nil
}

// LoadTokensWithTarget load all existing tokens for a given target
func LoadTokensWithTarget(repo repository.RepoCommon, target string) ([]*Token, error) {
	tokensIds, err := ListTokens(repo)
	if err != nil {
		return nil, err
	}

	var tokens []*Token
	for _, id := range tokensIds {
		token, err := LoadToken(repo, id)
		if err != nil {
			return nil, err
		}
		if token.Target == target {
			tokens = append(tokens, token)
		}
	}
	return tokens, nil
}

// TokenIdExist return wether token id exist or not
func TokenIdExist(repo repository.RepoCommon, id entity.Id) bool {
	_, err := LoadToken(repo, id)
	return err == nil
}

// TokenExist return wether there is a token with a certain value or not
func TokenExist(repo repository.RepoCommon, value string) bool {
	tokens, err := LoadTokens(repo)
	if err != nil {
		return false
	}
	for _, token := range tokens {
		if token.Value == value {
			return true
		}
	}
	return false
}

// TokenExistWithTarget same as TokenExist but restrict search for a given target
func TokenExistWithTarget(repo repository.RepoCommon, value string, target string) bool {
	tokens, err := LoadTokensWithTarget(repo, target)
	if err != nil {
		return false
	}
	for _, token := range tokens {
		if token.Value == value {
			return true
		}
	}
	return false
}

// StoreToken stores a token in the repo config
func StoreToken(repo repository.RepoCommon, token *Token) error {
	storeValueKey := fmt.Sprintf("git-bug.token.%s.%s", token.ID().String(), tokenValueKey)
	err := repo.GlobalConfig().StoreString(storeValueKey, token.Value)
	if err != nil {
		return err
	}

	storeTargetKey := fmt.Sprintf("git-bug.token.%s.%s", token.ID().String(), tokenTargetKey)
	err = repo.GlobalConfig().StoreString(storeTargetKey, token.Target)
	if err != nil {
		return err
	}

	createTimeKey := fmt.Sprintf("git-bug.token.%s.%s", token.ID().String(), tokenCreateTimeKey)
	return repo.GlobalConfig().StoreTimestamp(createTimeKey, token.CreateTime)
}

// RemoveToken removes a token from the repo config
func RemoveToken(repo repository.RepoCommon, id entity.Id) error {
	keyPrefix := fmt.Sprintf("git-bug.token.%s", id)
	return repo.GlobalConfig().RemoveAll(keyPrefix)
}

// LoadOrCreateToken will try to load a token matching the same value or create it
func LoadOrCreateToken(repo repository.RepoCommon, target, tokenValue string) (*Token, error) {
	tokens, err := LoadTokensWithTarget(repo, target)
	if err != nil {
		return nil, err
	}

	for _, token := range tokens {
		if token.Value == tokenValue {
			return token, nil
		}
	}

	token := NewToken(tokenValue, target)
	err = StoreToken(repo, token)
	if err != nil {
		return nil, err
	}

	return token, nil
}
