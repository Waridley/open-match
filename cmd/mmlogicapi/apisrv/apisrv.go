/*
package apisrv provides an implementation of the gRPC server defined in ../../../api/protobuf-spec/mmlogic.proto.
Most of the documentation for what these calls should do is in that file!

Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

*/

package apisrv

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/open-match/internal/metrics"
	mmlogic "github.com/GoogleCloudPlatform/open-match/internal/pb"
	"github.com/GoogleCloudPlatform/open-match/internal/set"
	redishelpers "github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis"
	"github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis/ignorelist"
	"github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis/redispb"
	log "github.com/sirupsen/logrus"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	"github.com/gomodule/redigo/redis"
	"github.com/spf13/viper"

	"go.opencensus.io/plugin/ocgrpc"
	"google.golang.org/grpc"
)

// Logrus structured logging setup
var (
	mlLogFields = log.Fields{
		"app":       "openmatch",
		"component": "mmlogic",
	}
	mlLog = log.WithFields(mlLogFields)
)

// MmlogicAPI implements mmlogic.ApiServer, the server generated by compiling
// the protobuf, by fulfilling the mmlogic.APIClient interface.
type MmlogicAPI struct {
	grpc *grpc.Server
	cfg  *viper.Viper
	pool *redis.Pool
}
type mmlogicAPI MmlogicAPI

// New returns an instantiated srvice
func New(cfg *viper.Viper, pool *redis.Pool) *MmlogicAPI {
	s := MmlogicAPI{
		pool: pool,
		grpc: grpc.NewServer(grpc.StatsHandler(&ocgrpc.ServerHandler{})),
		cfg:  cfg,
	}

	// Add a hook to the logger to auto-count log lines for metrics output thru OpenCensus
	log.AddHook(metrics.NewHook(MlLogLines, KeySeverity))

	// Register gRPC server
	mmlogic.RegisterMmLogicServer(s.grpc, (*mmlogicAPI)(&s))
	mlLog.Info("Successfully registered gRPC server")
	return &s
}

// Open starts the api grpc service listening on the configured port.
func (s *MmlogicAPI) Open() error {
	ln, err := net.Listen("tcp", ":"+s.cfg.GetString("api.mmlogic.port"))
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error": err.Error(),
			"port":  s.cfg.GetInt("api.mmlogic.port"),
		}).Error("net.Listen() error")
		return err
	}
	mlLog.WithFields(log.Fields{"port": s.cfg.GetInt("api.mmlogic.port")}).Info("TCP net listener initialized")

	go func() {
		err := s.grpc.Serve(ln)
		if err != nil {
			mlLog.WithFields(log.Fields{"error": err.Error()}).Error("gRPC serve() error")
		}
		mlLog.Info("serving gRPC endpoints")
	}()

	return nil
}

// GetProfile is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) GetProfile(c context.Context, profile *mmlogic.MatchObject) (*mmlogic.MatchObject, error) {

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Create context for tagging OpenCensus metrics.
	funcName := "GetProfile"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	// Get profile.
	mlLog.WithFields(log.Fields{"profileid": profile.Id}).Info("Attempting retreival of profile")
	err := redispb.UnmarshalFromRedis(c, s.pool, profile)
	mlLog.Warn("returned profile from redispb", profile)
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"profileid": profile.Id,
		}).Error("State storage error")

		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return profile, err
	}
	mlLog.WithFields(log.Fields{"profileid": profile.Id}).Debug("Retrieved profile from state storage")

	mlLog.Debug(profile)

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	//return out, err
	return profile, err

}

// CreateProposal is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) CreateProposal(c context.Context, prop *mmlogic.MatchObject) (*mmlogic.Result, error) {

	// Retreive configured redis keys.
	list := "proposed"
	proposalq := s.cfg.GetString("queues.proposals.name")

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Create context for tagging OpenCensus metrics.
	funcName := "CreateProposal"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	// Log what kind of results we received.
	cpLog := mlLog.WithFields(log.Fields{"id": prop.Id})
	if len(prop.Error) == 0 {
		cpLog.Info("writing MMF propsal to state storage")
	} else {
		cpLog.Info("writing MMF error to state storage")
	}

	// Write all non-id fields from the protobuf message to state storage.
	err := redispb.MarshalToRedis(c, s.pool, prop)
	if err != nil {
		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Result{Success: false, Error: err.Error()}, err
	}

	// Proposals need two more actions: players added to ignorelist, and adding
	// the proposalkey to the proposal queue for the evaluator to read.
	if len(prop.Error) == 0 {
		// look for players to add to the ignorelist
		cpLog.Info("parsing rosters")
		playerIDs := make([]string, 0)
		for _, roster := range prop.Rosters {
			playerIDs = append(playerIDs, getPlayerIdsFromRoster(roster)...)
		}

		// If players were on the roster, add them to the ignorelist
		if len(playerIDs) > 0 {
			cpLog.WithFields(log.Fields{
				"count":      len(playerIDs),
				"ignorelist": list,
			}).Info("adding players to ignorelist")

			err := ignorelist.Add(redisConn, list, playerIDs)
			if err != nil {
				cpLog.WithFields(log.Fields{
					"error":      err.Error(),
					"component":  "statestorage",
					"ignorelist": list,
				}).Error("State storage error")

				// record error.
				stats.Record(fnCtx, MlGrpcErrors.M(1))
				return &mmlogic.Result{Success: false, Error: err.Error()}, err
			}
		} else {
			cpLog.Warn("found no players in rosters, not adding any players to the proposed ignorelist")
		}

		// add propkey to proposalsq
		pqLog := cpLog.WithFields(log.Fields{
			"component": "statestorage",
			"queue":     proposalq,
		})
		pqLog.Info("adding propsal to queue")

		_, err = redisConn.Do("SADD", proposalq, prop.Id)
		if err != nil {
			pqLog.WithFields(log.Fields{"error": err.Error()}).Error("State storage error")

			// record error.
			stats.Record(fnCtx, MlGrpcErrors.M(1))
			return &mmlogic.Result{Success: false, Error: err.Error()}, err
		}
	}

	// Mark this MMF as finished by decrementing the concurrent MMFs.
	// This is used to trigger the evaluator early if all MMFs have finished
	// before its next scheduled run.
	cmLog := cpLog.WithFields(log.Fields{
		"component": "statestorage",
		"key":       "concurrentMMFs",
	})
	cmLog.Info("marking MMF finished for evaluator")
	_, err = redishelpers.Decrement(fnCtx, s.pool, "concurrentMMFs")
	if err != nil {
		cmLog.WithFields(log.Fields{"error": err.Error()}).Error("State storage error")

		// record error.
		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Result{Success: false, Error: err.Error()}, err
	}

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return &mmlogic.Result{Success: true, Error: ""}, err
}

// GetPlayerPool is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
// API_GetPlayerPoolServer returns mutiple PlayerPool messages - they should
// all be reassembled into one set on the calling side, as they are just
// paginated subsets of the player pool.
func (s *mmlogicAPI) GetPlayerPool(pool *mmlogic.PlayerPool, stream mmlogic.MmLogic_GetPlayerPoolServer) error {

	// TODO: quit if context is cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create context for tagging OpenCensus metrics.
	funcName := "GetPlayerPool"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	mlLog.WithFields(log.Fields{
		"filterCount": len(pool.Filters),
		"pool":        pool.Name,
		"funcName":    funcName,
	}).Info("attempting to retreive player pool from state storage")

	// One working Roster per filter in the set.  Combined at the end.
	filteredRosters := make(map[string][]string)
	// Temp store the results so we can also populate some field values in the final return roster.
	filteredResults := make(map[string]map[string]int64)
	overlap := make([]string, 0)
	fnStart := time.Now()

	// Loop over all filters, get results, combine
	for _, thisFilter := range pool.Filters {

		filterStart := time.Now()
		results, err := s.applyFilter(ctx, thisFilter)
		thisFilter.Stats = &mmlogic.Stats{Count: int64(len(results)), Elapsed: time.Since(filterStart).Seconds()}
		mlLog.WithFields(log.Fields{
			"count":      int64(len(results)),
			"elapsed":    time.Since(filterStart).Seconds(),
			"filterName": thisFilter.Name,
		}).Debug("Filter stats")

		if err != nil {
			mlLog.WithFields(log.Fields{"error": err.Error(), "filterName": thisFilter.Name}).Debug("Error applying filter")

			if len(results) == 0 {
				// One simple optimization here: check the count returned by a
				// ZCOUNT query for each filter before doing anything.  If any of the
				// filters return a ZCOUNT of 0, then the logical AND of all filters will
				// container no players and we can shortcircuit and quit.
				mlLog.WithFields(log.Fields{
					"count":      0,
					"filterName": thisFilter.Name,
					"pool":       pool.Name,
				}).Warn("returning empty pool")

				// Fill in the stats for this player pool.
				pool.Stats = &mmlogic.Stats{Count: int64(len(results)), Elapsed: time.Since(filterStart).Seconds()}

				// Send the empty pool and exit.
				if err = stream.Send(pool); err != nil {
					stats.Record(fnCtx, MlGrpcErrors.M(1))
					return err
				}
				stats.Record(fnCtx, MlGrpcRequests.M(1))
				return nil
			}

		}

		// Make an array of only the player IDs; used to do set.Unions and find the
		// logical AND
		m := make([]string, len(results))
		i := 0
		for playerID := range results {
			m[i] = playerID
			i++
		}

		// Store the array of player IDs as well as the full results for later
		// retrieval
		filteredRosters[thisFilter.Attribute] = m
		filteredResults[thisFilter.Attribute] = results
		overlap = m
	}

	// Player must be in every filtered pool to be returned
	for field, thesePlayers := range filteredRosters {
		overlap = set.Intersection(overlap, thesePlayers)

		_ = field
		//mlLog.WithFields(log.Fields{"count": len(overlap), "field": field}).Debug("Amount of overlap")
	}

	// Get contents of all ignore lists and remove those players from the pool.
	il, err := s.allIgnoreLists(ctx, &mmlogic.IlInput{})
	if err != nil {
		mlLog.Error(err)
	}
	mlLog.WithFields(log.Fields{"count": len(overlap)}).Debug("Pool size before applying ignorelists")
	mlLog.WithFields(log.Fields{"count": len(il)}).Debug("Ignorelist size")
	playerList := set.Difference(overlap, il) // removes ignorelist from the Roster
	mlLog.WithFields(log.Fields{"count": len(playerList)}).Debug("Final Pool size")

	// Reformat the playerList as a gRPC PlayerPool message. Send partial results as we go.
	// This is pretty agressive in the partial result 'page'
	// sizes it sends, and that is partially because it assumes you're running
	// everything on a local network.  If you aren't, you may need to tune this
	// pageSize.
	pageSize := s.cfg.GetInt("redis.results.pageSize")
	pageCount := int(math.Ceil((float64(len(playerList)) / float64(pageSize)))) // Divides and rounds up on any remainder
	//TODO: change if removing filtersets from rosters in favor of it being in pools
	partialRoster := mmlogic.Roster{Name: fmt.Sprintf("%v.partialRoster", pool.Name)}
	pool.Stats = &mmlogic.Stats{Count: int64(len(playerList)), Elapsed: time.Since(fnStart).Seconds()}
	for i := 0; i < len(playerList); i++ {
		// Check if we've filled in enough players to fill a page of results.
		if (i > 0 && i%pageSize == 0) || i == (len(playerList)-1) {
			pageName := fmt.Sprintf("%v.page%v/%v", pool.Name, i/pageSize+1, pageCount)
			poolChunk := &mmlogic.PlayerPool{
				Name:    pageName,
				Filters: pool.Filters,
				Stats:   pool.Stats,
				Roster:  &partialRoster,
			}
			if err = stream.Send(poolChunk); err != nil {
				stats.Record(fnCtx, MlGrpcErrors.M(1))
				return err
			}
			partialRoster.Players = []*mmlogic.Player{}
		}

		// Add one additional player result to the partial pool.
		player := &mmlogic.Player{Id: playerList[i], Attributes: []*mmlogic.Player_Attribute{}}
		// Collect all the filtered attributes into the player protobuf.
		for attribute, fr := range filteredResults {
			if value, ok := fr[playerList[i]]; ok {
				player.Attributes = append(player.Attributes, &mmlogic.Player_Attribute{Name: attribute, Value: value})
			}
		}
		partialRoster.Players = append(partialRoster.Players, player)

	}

	mlLog.WithFields(log.Fields{"count": len(playerList), "pool": pool.Name}).Debug("player pool streaming complete")

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return nil
}

// applyFilter is a sequential query of every entry in the Redis sorted set
// that fall beween the minimum and maximum values passed in through the filter
// argument.  This can be likely sped up later using concurrent access, but
// with small enough player pools (less than the 'redis.queryArgs.count' config
// parameter) the amount of work is identical, so this is fine as a starting point.
// If the provided field is not indexed or the provided range is too large, a nil result
// is returned and this filter should be disregarded when applying filter overlaps.
func (s *mmlogicAPI) applyFilter(c context.Context, filter *mmlogic.Filter) (map[string]int64, error) {

	type pName string
	pool := make(map[string]int64)

	// Default maximum value is positive infinity (i.e. highest possible number in redis)
	// https://redis.io/commands/zrangebyscore
	maxv := strconv.FormatInt(filter.Maxv, 10) // Convert int64 to a string
	if filter.Maxv == 0 {                      // No max specified, set to +inf
		maxv = "+inf"
	}

	mlLog.WithFields(log.Fields{"filterField": filter.Attribute}).Debug("In applyFilter")

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Check how many expected matches for this filter before we start retrieving.
	cmd := "ZCOUNT"
	count, err := redis.Int64(redisConn.Do(cmd, filter.Attribute, filter.Minv, maxv))
	//DEBUG: count, err := redis.Int64(redisConn.Do(cmd, "BLARG", filter.Minv, maxv))
	mlLog := mlLog.WithFields(log.Fields{
		"query": cmd,
		"field": filter.Attribute,
		"minv":  filter.Minv,
		"maxv":  maxv,
		"count": count,
	})
	if err != nil {
		mlLog.WithFields(log.Fields{"error": err.Error()}).Error("state storage error")
		return nil, err
	}

	if count == 0 {
		err = errors.New("filter applies to no players")
		mlLog.Error(err.Error())
		return nil, err
	} else if count > 500000 {
		// 500,000 results is an arbitrary number; OM doesn't encourage
		// patterns where MMFs look at this large of a pool.
		err = errors.New("filter applies to too many players")
		mlLog.Error(err.Error())
		for i := 0; i < int(count); i++ {
			// Send back an empty pool, used by the calling function to calculate the number of results
			pool[strconv.Itoa(i)] = 0
		}
		return pool, err
	} else if count < 100000 {
		mlLog.Info("filter processed")
	} else {
		// Send a warning to the logs.
		mlLog.Warn("filter applies to a large number of players")
	}

	// Amount of results look okay and no redis error, begin
	// var init for player retrieval
	cmd = "ZRANGEBYSCORE"
	offset := 0

	// Loop, retrieving players in chunks.
	for len(pool) == offset {
		results, err := redis.Int64Map(redisConn.Do(cmd, filter.Attribute, filter.Minv, maxv, "WITHSCORES", "LIMIT", offset, s.cfg.GetInt("redis.queryArgs.count")))
		if err != nil {
			mlLog.WithFields(log.Fields{
				"query":  cmd,
				"field":  filter.Attribute,
				"minv":   filter.Minv,
				"maxv":   maxv,
				"offset": offset,
				"count":  s.cfg.GetInt("redis.queryArgs.count"),
				"error":  err.Error(),
			}).Error("statestorage error")
		}

		// Increment the offset for the next query by the 'count' config value
		offset = offset + s.cfg.GetInt("redis.queryArgs.count")

		// Add all results to this player pool
		for k, v := range results {
			if _, ok := pool[k]; ok {
				// Redis returned the same player more than once; this is not
				// actually a problem, it just indicates that players are being
				// added/removed from the index as it is queried.  We take the
				// tradeoff in consistency for speed, as it won't cause issues
				// in matchmaking results as long as ignorelists are respected.
				offset--
			}
			pool[k] = v
		}
	}

	// Log completion and return
	//mlLog.WithFields(log.Fields{
	//	"poolSize": len(pool),
	//	"field":    filter.Attribute,
	//	"minv":     filter.Minv,
	//	"maxv":     maxv,
	//}).Debug("Player pool filter processed")

	return pool, nil
}

// GetAllIgnoredPlayers is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
// This is a wrapper around allIgnoreLists, and converts the []string return
// value of that function to a gRPC Roster message to send out over the wire.
func (s *mmlogicAPI) GetAllIgnoredPlayers(c context.Context, in *mmlogic.IlInput) (*mmlogic.Roster, error) {

	// Create context for tagging OpenCensus metrics.
	funcName := "GetAllIgnoredPlayers"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	il, err := s.allIgnoreLists(c, in)

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return createRosterfromPlayerIds(il), err
}

// ListIgnoredPlayers is this service's implementation of the gRPC call defined in
// mmlogicapi/proto/mmlogic.proto
func (s *mmlogicAPI) ListIgnoredPlayers(c context.Context, olderThan *mmlogic.IlInput) (*mmlogic.Roster, error) {

	// TODO: is this supposed to able to take any list?
	ilName := "proposed"

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	// Create context for tagging OpenCensus metrics.
	funcName := "ListIgnoredPlayers"
	fnCtx, _ := tag.New(c, tag.Insert(KeyMethod, funcName))

	mlLog.WithFields(log.Fields{"ignorelist": ilName}).Info("Attempting to get ignorelist")

	// retreive ignore list
	il, err := ignorelist.Retrieve(redisConn, s.cfg, ilName)
	if err != nil {
		mlLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
			"key":       ilName,
		}).Error("State storage error")

		stats.Record(fnCtx, MlGrpcErrors.M(1))
		return &mmlogic.Roster{}, err
	}
	// TODO: fix this
	mlLog.Debug(fmt.Sprintf("Retreival success %v", il))

	stats.Record(fnCtx, MlGrpcRequests.M(1))
	return createRosterfromPlayerIds(il), err
}

// allIgnoreLists combines all the ignore lists and returns them.
func (s *mmlogicAPI) allIgnoreLists(c context.Context, in *mmlogic.IlInput) (allIgnored []string, err error) {

	// Get redis connection from pool
	redisConn := s.pool.Get()
	defer redisConn.Close()

	mlLog.Info("Attempting to get and combine ignorelists")

	// Loop through all ignorelists configured in the config file.
	for il := range s.cfg.GetStringMap("ignoreLists") {
		ilCfg := s.cfg.Sub(fmt.Sprintf("ignoreLists.%v", il))
		thisIl, err := ignorelist.Retrieve(redisConn, ilCfg, il)
		if err != nil {
			panic(err)
		}

		// Join this ignorelist to the others we've retrieved
		allIgnored = set.Union(allIgnored, thisIl)
	}

	return allIgnored, err
}

// Functions for getting or setting player IDs to/from rosters
// Probably should get moved to an internal module in a future version.
func getPlayerIdsFromRoster(r *mmlogic.Roster) []string {
	playerIDs := make([]string, 0)
	for _, p := range r.Players {
		playerIDs = append(playerIDs, p.Id)
	}
	return playerIDs

}

func createRosterfromPlayerIds(playerIDs []string) *mmlogic.Roster {

	players := make([]*mmlogic.Player, 0)
	for _, id := range playerIDs {
		players = append(players, &mmlogic.Player{Id: id})
	}
	return &mmlogic.Roster{Players: players}

}
