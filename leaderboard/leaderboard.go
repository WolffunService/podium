// podium
// https://github.com/topfreegames/podium
// Licensed under the MIT license:
// http://www.opensource.org/licenses/mit-license
// Copyright © 2016 Top Free Games <backend@tfgco.com>
// Forked from
// https://github.com/dayvson/go-leaderboard
// Copyright © 2013 Maxwell Dayvson da Silva

package leaderboard

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis"
	"github.com/topfreegames/extensions/redis/interfaces"
	"github.com/topfreegames/podium/util"
	"go.uber.org/zap"
)

//MemberNotFoundError indicates member was not found in Redis
type MemberNotFoundError struct {
	LeaderboardID string
	MemberID      string
}

func (e *MemberNotFoundError) Error() string {
	return fmt.Sprintf("Could not find data for member %s in leaderboard %s.", e.MemberID, e.LeaderboardID)
}

//NewMemberNotFound returns a new error for member not found
func NewMemberNotFound(leaderboardID, memberID string) *MemberNotFoundError {
	return &MemberNotFoundError{
		LeaderboardID: leaderboardID,
		MemberID:      memberID,
	}
}

// Member maps an member identified by their publicID to their score and rank
type Member struct {
	PublicID     string `json:"publicID"`
	Score        int64  `json:"score"`
	Rank         int    `json:"rank"`
	PreviousRank int    `json:"previousRank"`
	ExpireAt     int    `json:"expireAt"`
}

//Members are a list of member
type Members []*Member

func (slice Members) Len() int {
	return len(slice)
}

func (slice Members) Less(i, j int) bool {
	return slice[i].Rank < slice[j].Rank
}

func (slice Members) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

// Leaderboard identifies a leaderboard with given redis client
type Leaderboard struct {
	Logger      zap.Logger
	RedisClient interfaces.RedisClient
	PublicID    string
	PageSize    int
}

func getSetScoreScript(operation string) *redis.Script {
	return redis.NewScript(fmt.Sprintf(`
		-- Script params:
		-- KEYS[1] is the name of the leaderboard
		-- ARGV[1] are the Members JSON
		-- ARGV[2] is the leaderboard's expiration
		-- ARGV[3] defines if the previous rank should be returned
		-- ARGV[4] defines the ttl of the player score
		-- ARGV[5] defines the current unix timestamp

		-- creates leaderboard or just sets score of member
		local key_pairs = {}
		local members = cjson.decode(ARGV[1])
		local score_ttl = ARGV[4]
		if score_ttl == nil or score_ttl == "" then
			score_ttl = "inf"
		end
		for i,mem in ipairs(members) do
			table.insert(key_pairs, tonumber(mem["score"]))
			table.insert(key_pairs, mem["publicID"])
			if (ARGV[3] == "1") then
				mem["previousRank"] = tonumber(redis.call("ZREVRANK", KEYS[1], mem["publicID"])) or -2
			end
		end
		redis.call("%s", KEYS[1], unpack(key_pairs))

		-- If expiration is required set expiration
		if (ARGV[2] ~= "-1") then
			local expiration = redis.call("TTL", KEYS[1])
			if (expiration == -2) then
				return redis.error_reply("Leaderboard Set was not created in %s! Don't know how to proceed.")
			end
			if (expiration == -1) then
				redis.call("EXPIREAT", KEYS[1], ARGV[2])
			end
		end

		local expire_at = "nil"
		if (score_ttl ~= "inf") then
			local expiration_set_key = KEYS[1]..":ttl"
			expire_at = ARGV[5] + score_ttl
			key_pairs = {}
			for i,mem in ipairs(members) do
				table.insert(key_pairs, expire_at)
				table.insert(key_pairs, mem["publicID"])
			end
			redis.call("ZADD", expiration_set_key, unpack(key_pairs))
			redis.call("SADD", "expiration-sets", expiration_set_key)
		end

		-- return updated rank of member
		local result = {}
		for i,mem in ipairs(members) do
			table.insert(result, mem["publicID"])
			table.insert(result, tonumber(redis.call("ZREVRANK", KEYS[1], mem["publicID"])))
			table.insert(result, tonumber(redis.call("ZSCORE", KEYS[1], mem["publicID"])))
			if ARGV[3] == "1" then
				table.insert(result, mem["previousRank"])
			else
				table.insert(result, -1)
			end
			table.insert(result, expire_at)
		end
		return result
	`, operation, operation))
}

//GetMembersByRange for a given leaderboard
func GetMembersByRange(redisClient interfaces.RedisClient, leaderboard string, startOffset int, endOffset int, order string, l zap.Logger) ([]*Member, error) {
	cli := redisClient
	l.Debug(
		"Retrieving members for range.",
		zap.Int("startOffset", startOffset),
		zap.Int("endOffset", endOffset),
	)

	var values []redis.Z
	var err error
	if strings.Compare(order, "desc") == 0 {
		values, err = cli.ZRevRangeWithScores(leaderboard, int64(startOffset), int64(endOffset)).Result()
	} else {
		values, err = cli.ZRangeWithScores(leaderboard, int64(startOffset), int64(endOffset)).Result()
	}
	if err != nil {
		l.Error(
			"Retrieval of members for range failed.",
			zap.Int("startOffset", startOffset),
			zap.Int("endOffset", endOffset),
			zap.Error(err),
		)
		return nil, err
	}
	l.Debug(
		"Retrieving members for range succeeded.",
		zap.Int("startOffset", startOffset),
		zap.Int("endOffset", endOffset),
	)

	l.Debug("Building details of leaderboard members...")
	members := make([]*Member, len(values))
	for i := 0; i < len(members); i++ {
		publicID := values[i].Member.(string)
		score := values[i].Score
		nMember := Member{PublicID: publicID, Score: int64(score), Rank: int(startOffset + i + 1)}
		members[i] = &nMember
	}
	l.Debug("Retrieval of leaderboard members' details succeeded.")

	return members, nil
}

// getMemberIDWithClosestScore returns a member in a given leaderboard with score >= the score provided
func getMemberIDWithClosestScore(redisClient interfaces.RedisClient, leaderboard string, score int64, l zap.Logger) (string, error) {
	cli := redisClient
	l.Debug(
		"Retrieving member with closest score.",
		zap.Int64("score", score),
	)

	values, err := cli.ZRevRangeByScore(leaderboard, redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(score, 10), Offset: 0, Count: 1}).Result()

	if err != nil {
		l.Error(
			"Retrieval of member with closes score failed.",
			zap.Int64("score", score),
			zap.Error(err),
		)
		return "", err
	}

	l.Debug(
		"Retrieved member with closest score succeeded.",
		zap.Int64("score", score),
	)

	if len(values) < 1 {
		return "", nil
	}

	return values[0], nil
}

// NewLeaderboard creates a new Leaderboard with given settings, ID and pageSize
func NewLeaderboard(redisClient interfaces.RedisClient, publicID string, pageSize int, logger zap.Logger) *Leaderboard {
	return &Leaderboard{RedisClient: redisClient, PublicID: publicID, PageSize: pageSize, Logger: logger}
}

//AddToLeaderboardSet adds a score to a leaderboard set respecting expiration
func (lb *Leaderboard) AddToLeaderboardSet(members Members, prevRank bool, scoreTTL string) (error) {
	cli := lb.RedisClient

	l := lb.Logger.With(
		zap.String("operation", "AddToLeaderboardSet"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.Object("members", members),
		zap.String("scoreTTL", scoreTTL),
	)

	l.Debug("Calculating expiration for leaderboard...")
	expireAt, err := util.GetExpireAt(lb.PublicID)
	if err != nil {
		l.Error("Could not get expiration.", zap.Error(err))
		return err
	}
	l.Debug("Expiration calculated successfully.", zap.Int64("expiration", expireAt))

	script := getSetScoreScript("ZADD")

	l.Debug("Updating rank for members.")
	jsonMembers, _ := json.Marshal(members)
	newRanks, err := script.Run(cli, []string{lb.PublicID}, jsonMembers, expireAt, prevRank, scoreTTL, time.Now().Unix()).Result()
	if err != nil {
		l.Error("Failed to update rank for members.", zap.Error(err))
		return err
	}

	res := newRanks.([]interface{})
	for i := 0; i < len(res); i += 5 {
		memberIndex := i/5
		members[memberIndex].PublicID = res[i].(string)
		members[memberIndex].Score = res[i+2].(int64)
		members[memberIndex].Rank = int(res[i+1].(int64)) + 1
		members[memberIndex].PreviousRank = int(res[i+3].(int64)) + 1
		if scoreTTL != "" && scoreTTL != "inf" {
			members[memberIndex].ExpireAt = int(res[i+4].(int64))
		}
	}

	l.Debug("Rank for members retrieved successfully.")
	return err
}

// IncrementMemberScore sets the score to the member with the given ID
func (lb *Leaderboard) IncrementMemberScore(memberID string, increment int, scoreTTL string) (*Member, error) {
	l := lb.Logger.With(
		zap.String("operation", "IncrementMemberScore"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.String("memberID", memberID),
		zap.String("scoreTTL", scoreTTL),
		zap.Int("increment", increment),
	)
	l.Debug("Setting member score increment...")
	cli := lb.RedisClient

	script := getSetScoreScript("ZINCRBY")

	l.Debug("Calculating expiration for leaderboard...")
	expireAt, err := util.GetExpireAt(lb.PublicID)
	if err != nil {
		l.Error("Could not get expiration.", zap.Error(err))
		return nil, err
	}
	l.Debug("Expiration calculated successfully.", zap.Int64("expiration", expireAt))

	l.Debug("Incrementing score for member...")

	jsonMembers, _ := json.Marshal(Members{&Member{PublicID: memberID, Score: int64(increment)}})
	// TODO use prevRank instead of hard coded false
	result, err := script.Run(cli, []string{lb.PublicID}, jsonMembers, expireAt, false, scoreTTL, time.Now().Unix()).Result()
	if err != nil {
		l.Error("Could not increment score for member.", zap.Error(err))
		return nil, err
	}
	l.Debug("Increment result from redis", zap.Object("result", result))
	rank := int(result.([]interface{})[1].(int64)) + 1
	score := result.([]interface{})[2].(int64)

	l.Debug("Member score increment set successfully.")
	nMember := Member{PublicID: memberID, Score: score, Rank: rank}
	if scoreTTL != "" && scoreTTL != "inf" {
		nMember.ExpireAt = int(result.([]interface{})[4].(int64))
	}
	return &nMember, err
}

// SetMemberScore sets the score to the member with the given ID
func (lb *Leaderboard) SetMemberScore(memberID string, score int64, prevRank bool, scoreTTL string) (*Member, error) {
	members := Members{&Member{PublicID: memberID, Score: score}}
	err := lb.SetMembersScore(members, prevRank, scoreTTL)
	return members[0], err
}

// SetMembersScore sets the scores of the members with the given IDs
func (lb *Leaderboard) SetMembersScore(members Members, prevRank bool, scoreTTL string) (error) {
	l := lb.Logger.With(
		zap.String("operation", "SetMembersScore"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.String("scoreTTL", scoreTTL),
		zap.Object("members", members),
	)
	l.Debug("Setting member(s) score...")

	err := lb.AddToLeaderboardSet(members, prevRank, scoreTTL)
	if err == nil {
		l.Debug("Member(s) score set successfully.")
	}

	return err
}

// TotalMembers returns the total number of members in a given leaderboard
func (lb *Leaderboard) TotalMembers() (int, error) {
	l := lb.Logger.With(
		zap.String("operation", "TotalMembers"),
		zap.String("leaguePublicID", lb.PublicID),
	)
	cli := lb.RedisClient

	l.Debug("Retrieving total members...")
	total, err := cli.ZCard(lb.PublicID).Result()
	if err != nil {
		l.Error("Retrieval of total members failed.", zap.Error(err))
		return 0, err
	}
	l.Debug("Total members of leaderboard retrieved successfully.")
	return int(total), nil
}

// RemoveMembers removes the members with the given publicIDs from the leaderboard
func (lb *Leaderboard) RemoveMembers(memberIDs []interface{}) error {
	l := lb.Logger.With(
		zap.String("operation", "RemoveMembers"),
		zap.String("leaguePublicID", lb.PublicID),
	)

	cli := lb.RedisClient

	l.Debug("Removing members from leaderboard...")

	_, err := cli.ZRem(lb.PublicID, memberIDs...).Result()
	if err != nil {
		l.Error("Members removal failed...", zap.Error(err))
		return err
	}
	l.Debug("Members removed successfully.")
	return nil
}

// RemoveMember removes the member with the given publicID from the leaderboard
func (lb *Leaderboard) RemoveMember(memberID string) error {
	l := lb.Logger.With(
		zap.String("operation", "RemoveMember"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.String("memberID", memberID),
	)

	cli := lb.RedisClient

	l.Debug("Removing member from leaderboard...")

	_, err := cli.ZRem(lb.PublicID, memberID).Result()
	if err != nil {
		l.Error("Member removal failed...", zap.Error(err))
		return err
	}
	l.Debug("Member removed successfully.")
	return nil
}

// TotalPages returns the number of pages of the leaderboard
func (lb *Leaderboard) TotalPages() (int, error) {
	l := lb.Logger.With(
		zap.String("operation", "TotalPages"),
		zap.String("leaguePublicID", lb.PublicID),
	)

	cli := lb.RedisClient

	l.Debug("Retrieving number of pages for leaderboard.")
	pages := 0
	total, err := cli.ZCard(lb.PublicID).Result()
	if err != nil {
		l.Error("Number of pages could not be retrieved.", zap.Error(err))
		return 0, err
	}
	pages = int(math.Ceil(float64(total) / float64(lb.PageSize)))
	l.Debug("Number of pages for leaderboard retrieved successfully.", zap.Int("numberOfPages", pages))
	return pages, nil
}

// GetMember returns the score and the rank of the member with the given ID
func (lb *Leaderboard) GetMember(memberID string, order string, includeTTL bool) (*Member, error) {
	l := lb.Logger.With(
		zap.String("operation", "GetMember"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.String("memberID", memberID),
	)

	if order != "desc" && order != "asc" {
		order = "desc"
	}

	cli := lb.RedisClient
	var operations = map[string]string{
		"rank_desc": "ZREVRANK",
		"rank_asc":  "ZRANK",
	}

	l.Debug("Getting member information...")
	script := redis.NewScript(`
		-- Script params:
		-- KEYS[1] is the name of the leaderboard
		-- KEYS[2] is member's public ID
		-- ARGV[1] is a bool indicating whether the score ttl should be retrieved

        local score_ttl = ARGV[1] == "true"
		-- gets rank of the member
		local rank = redis.call("` + operations["rank_"+order] + `", KEYS[1], KEYS[2])
		local score = redis.call("ZSCORE", KEYS[1], KEYS[2])
        if score_ttl then
			local expire_at = redis.call("ZSCORE", KEYS[1]..":ttl", KEYS[2])
			return {rank,score,expire_at}
        end
		return {rank,score}
	`)

	result, err := script.Run(cli, []string{lb.PublicID, memberID}, strconv.FormatBool(includeTTL)).Result()
	if err != nil {
		l.Error("Getting member information failed.", zap.Error(err))
		return nil, err
	}

	res := result.([]interface{})

	if res[0] == nil || res[1] == nil {
		l.Error("Could not find member.", zap.Error(err))
		return nil, NewMemberNotFound(lb.PublicID, memberID)
	}

	rank := int(res[0].(int64))
	score, _ := strconv.ParseInt(res[1].(string), 10, 64)

	l.Debug("Member information found.", zap.Int("rank", rank), zap.Int64("score", score))
	nMember := Member{PublicID: memberID, Score: score, Rank: rank + 1}
	if includeTTL {
		if expireAtStr, ok := res[2].(string); ok {
			expireAtParsed, _ := strconv.ParseInt(expireAtStr, 10, 32)
			nMember.ExpireAt = int(expireAtParsed)
		}
	}
	return &nMember, nil
}

// GetMembers returns the score and the rank of the members with the given IDs
func (lb *Leaderboard) GetMembers(memberIDs []string, order string, includeTTL bool) ([]*Member, error) {
	l := lb.Logger.With(
		zap.String("operation", "GetMembers"),
		zap.String("leaguePublicID", lb.PublicID),
	)

	cli := lb.RedisClient

	var operations = map[string]string{
		"rank_desc": "ZREVRANK",
		"rank_asc":  "ZRANK",
	}

	l.Debug("Getting members information...")
	script := redis.NewScript(`
		-- Script params:
		-- KEYS[1] is the name of the leaderboard
		-- ARGV[1] is member's public IDs
		-- ARGV[2] is a bool indicating whether the score ttl should be retrieved

        local score_ttl = ARGV[2] == "true"
		local members = {}

		for publicID in string.gmatch(ARGV[1], '([^,]+)') do
			-- gets rank of the member
			local rank = redis.call("` + operations["rank_"+order] + `", KEYS[1], publicID)
			local score = redis.call("ZSCORE", KEYS[1], publicID)

			table.insert(members, publicID)
			table.insert(members, rank)
			table.insert(members, score)

			if score_ttl then
				local expire_at = redis.call("ZSCORE", KEYS[1]..":ttl", publicID)
				table.insert(members, expire_at)
			else
				table.insert(members, "nil")
			end
		end

		return members
	`)

	result, err := script.Run(cli, []string{lb.PublicID}, strings.Join(memberIDs, ","), strconv.FormatBool(includeTTL)).Result()
	if err != nil {
		l.Error("Getting members information failed.", zap.Error(err))
		return nil, err
	}

	res := result.([]interface{})
	members := Members{}
	for i := 0; i < len(res); i += 4 {
		memberPublicID := res[i].(string)
		if res[i+1] == nil || res[i+2] == nil {
			continue
		}

		rank := int(res[i+1].(int64)) + 1
		score, _ := strconv.ParseInt(res[i+2].(string), 10, 64)
		member := &Member{
			PublicID: memberPublicID,
			Score:    score,
			Rank:     rank,
		}
		if includeTTL {
			if expireAtStr, ok := res[i+3].(string); ok {
				expireAtParsed, _ := strconv.ParseInt(expireAtStr, 10, 32)
				member.ExpireAt = int(expireAtParsed)
			}
		}

		members = append(members, member)
	}

	l.Debug("Members information found.")
	sort.Sort(members)
	return members, nil
}

// GetAroundMe returns a page of results centered in the member with the given ID
func (lb *Leaderboard) GetAroundMe(memberID string, order string, getLastIfNotFound bool) ([]*Member, error) {
	l := lb.Logger.With(
		zap.String("operation", "GetAroundMe"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.String("memberID", memberID),
	)

	if order != "desc" && order != "asc" {
		order = "desc"
	}

	l.Debug("Getting information about members around a specific member...")
	currentMember, err := lb.GetMember(memberID, order, false)
	_, memberNotFound := err.(*MemberNotFoundError)
	if (err != nil && !memberNotFound) || (memberNotFound && !getLastIfNotFound) {
		return nil, err
	}

	totalMembers, err := lb.TotalMembers()
	if err != nil {
		return nil, err
	}

	if memberNotFound && getLastIfNotFound {
		currentMember = &Member{PublicID: memberID, Score: 0, Rank: totalMembers + 1}
	}

	startOffset := currentMember.Rank - (lb.PageSize / 2)
	if startOffset < 0 {
		startOffset = 0
	}
	endOffset := (startOffset + lb.PageSize) - 1
	if totalMembers < endOffset {
		endOffset = totalMembers
		startOffset = endOffset - lb.PageSize
		if startOffset < 0 {
			startOffset = 0
		}
	}

	members, err := GetMembersByRange(lb.RedisClient, lb.PublicID, startOffset, endOffset, order, l)
	if err != nil {
		l.Error("Failed to retrieve information around a specific member.", zap.Error(err))
		return nil, err
	}

	l.Debug("Retrieved information around member successfully.")
	return members, nil
}

// GetAroundScore returns a page of results centered in the score provided
func (lb *Leaderboard) GetAroundScore(score int64, order string) ([]*Member, error) {
	l := lb.Logger.With(
		zap.String("operation", "GetAroundScore"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.Int64("score", score),
	)

	//GetMembersByRange(lb.RedisClient, lb.PublicID, startOffset, endOffset, order, l)
	memberID, err := getMemberIDWithClosestScore(lb.RedisClient, lb.PublicID, score, l)
	if err != nil {
		l.Error(fmt.Sprintf("Failed to retrieve information around a specific score (%d).", score), zap.Error(err))
		return nil, err
	}

	return lb.GetAroundMe(memberID, order, true)
}

// GetRank returns the rank of the member with the given ID
func (lb *Leaderboard) GetRank(memberID string, order string) (int, error) {
	l := lb.Logger.With(
		zap.String("operation", "GetRank"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.String("memberID", memberID),
	)

	cli := lb.RedisClient

	l.Debug("Getting rank of specific member...")
	var rank int64
	var err error
	if order == "desc" {
		rank, err = cli.ZRevRank(lb.PublicID, memberID).Result()
	} else {
		rank, err = cli.ZRank(lb.PublicID, memberID).Result()
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "redis: nil") {
			l.Error("Member was not found in specified leaderboard.", zap.Error(err))
			return -1, NewMemberNotFound(lb.PublicID, memberID)
		}

		l.Error("Failed to retrieve rank of specific member.", zap.Error(err))
		return -1, err
	}
	l.Debug("Rank retrieval succeeded.")
	return int(rank + 1), nil
}

// GetLeaders returns a page of members with rank and score
func (lb *Leaderboard) GetLeaders(page int, order string) ([]*Member, error) {
	l := lb.Logger.With(
		zap.String("operation", "GetLeaders"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.Int("page", page),
	)

	if page < 1 {
		page = 1
	}

	totalPages, err := lb.TotalPages()
	if err != nil {
		return nil, err
	}

	if page > totalPages {
		return make([]*Member, 0), nil
	}

	redisIndex := page - 1
	startOffset := redisIndex * lb.PageSize
	endOffset := (startOffset + lb.PageSize) - 1
	return GetMembersByRange(lb.RedisClient, lb.PublicID, startOffset, endOffset, order, l)
}

//GetTopPercentage of members in the leaderboard.
func (lb *Leaderboard) GetTopPercentage(amount, maxMembers int, order string) ([]*Member, error) {
	l := lb.Logger.With(
		zap.String("operation", "GetTopPercentage"),
		zap.String("leaguePublicID", lb.PublicID),
		zap.Int("amount", amount),
	)

	if amount < 1 || amount > 100 {
		err := fmt.Errorf("Percentage must be a valid integer between 1 and 100.")
		l.Error(err.Error(), zap.Error(err))
		return nil, err
	}

	if order != "desc" && order != "asc" {
		order = "desc"
	}

	cli := lb.RedisClient
	var operations = map[string]string{
		"range_desc": "ZREVRANGE",
		"rank_desc":  "ZREVRANK",
		"range_asc":  "ZRANGE",
		"rank_asc":   "ZRANK",
	}

	l.Debug("Getting top percentage of members...")
	script := redis.NewScript(`
		-- Script params:
		-- KEYS[1] is the name of the leaderboard
		-- ARGV[1] is the desired percentage (0.0 to 1.0)
		-- ARGV[2] is the maximum number of members returned

		local totalNumber = redis.call("ZCARD", KEYS[1])
		local numberOfMembers = math.floor(ARGV[1] * totalNumber)
		if (numberOfMembers < 1) then
			numberOfMembers = 1
		end

		if (numberOfMembers > math.floor(ARGV[2])) then
			numberOfMembers = math.floor(ARGV[2])
		end

		local members = redis.call("` + operations["range_"+order] + `", KEYS[1], 0, numberOfMembers - 1, "WITHSCORES")
		local fullMembers = {}

		for index=1, #members, 2 do
			local publicID = members[index]
			local score = members[index + 1]
		 	local rank = redis.call("` + operations["rank_"+order] + `", KEYS[1], publicID)

			table.insert(fullMembers, publicID)
			table.insert(fullMembers, rank)
			table.insert(fullMembers, score)
		end

		return fullMembers
	`)

	result, err := script.Run(cli, []string{lb.PublicID}, float64(amount)/100.0, maxMembers).Result()

	if err != nil {
		l.Error("Getting top percentage of members failed.", zap.Error(err))
		return nil, err
	}

	res := result.([]interface{})
	members := []*Member{}

	for i := 0; i < len(res); i += 3 {
		memberPublicID := res[i].(string)

		rank := int(res[i+1].(int64)) + 1
		score, _ := strconv.ParseInt(res[i+2].(string), 10, 64)

		members = append(members, &Member{
			PublicID: memberPublicID,
			Score:    score,
			Rank:     rank,
		})
	}

	l.Debug("Top percentage of members retrieved successfully.")

	return members, nil
}

// RemoveLeaderboard removes a leaderboard from redis
func (lb *Leaderboard) RemoveLeaderboard() error {
	l := lb.Logger.With(
		zap.String("operation", "RemoveLeaderboard"),
		zap.String("leaguePublicID", lb.PublicID),
	)

	l.Debug("Removing leaderboard...")
	cli := lb.RedisClient

	_, err := cli.Del(lb.PublicID).Result()
	if err != nil {
		l.Error("Failed to remove leaderboard.", zap.Error(err))
		return err
	}

	l.Debug("Leaderboard removed.")
	return nil
}
