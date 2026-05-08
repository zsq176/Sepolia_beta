package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"
	"market-maker-service/internal/domain"

	"github.com/redis/go-redis/v9"
)

type auditEnvelope struct {
	Type     string                  `json:"type"`
	Decision *decisionPersistRequest `json:"decision,omitempty"`
	Result   *domain.ExecutionResult `json:"result,omitempty"`
}

type decisionPersistRequest struct {
	InstrumentID string  `json:"instrument_id"`
	PoolPrice    float64 `json:"pool_price"`
	TargetPrice  float64 `json:"target_price"`
	Deviation    float64 `json:"deviation"`
	NotionalUSD  float64 `json:"notional_usd"`
	QualityLevel string  `json:"quality_level"`
	State        string  `json:"state"`
	Allowed      bool    `json:"allowed"`
	Reason       string  `json:"reason"`
	Timestamp    int64   `json:"timestamp"`
}

func (s *Service) initRedis() {
	if s.cfg == nil || s.cfg.RedisAddr == "" {
		return
	}
	// Avoid noisy go-redis pool retry logs when Redis is not up.
	conn, err := net.DialTimeout("tcp", s.cfg.RedisAddr, 700*time.Millisecond)
	if err != nil {
		log.Printf("[exec] redis disabled (dial failed): %v", err)
		return
	}
	_ = conn.Close()
	s.rdb = redis.NewClient(&redis.Options{
		Addr:     s.cfg.RedisAddr,
		Password: s.cfg.RedisPass,
		DB:       int(s.cfg.RedisDB),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		log.Printf("[exec] redis disabled (ping failed): %v", err)
		_ = s.rdb.Close()
		s.rdb = nil
		return
	}
	s.redisQueue = s.cfg.RedisQueue
	if s.redisQueue == "" {
		s.redisQueue = "mm:audit:events"
	}
	s.redisLockTTL = 15 * time.Second
	if s.cfg.RedisLockTTL > 0 {
		s.redisLockTTL = time.Duration(s.cfg.RedisLockTTL) * time.Second
	}
	s.auditCancel = make(chan struct{})
	go s.auditConsumerLoop()
	log.Printf("[exec] redis enabled (queue=%s lock_ttl=%s)", s.redisQueue, s.redisLockTTL)
}

func (s *Service) closeRedis() {
	if s.auditCancel != nil {
		close(s.auditCancel)
		s.auditCancel = nil
	}
	if s.rdb != nil {
		_ = s.rdb.Close()
		s.rdb = nil
	}
}

func (s *Service) enqueueDecision(req *decisionPersistRequest) {
	if s.rdb == nil {
		s.persistDecisionSync(req)
		return
	}
	env := auditEnvelope{Type: "decision", Decision: req}
	if err := s.enqueueEnvelope(&env); err != nil {
		log.Printf("[exec] redis enqueue decision failed, fallback sync: %v", err)
		s.persistDecisionSync(req)
	}
}

func (s *Service) enqueueResult(r *domain.ExecutionResult) {
	if s.rdb == nil {
		s.persistSync(r)
		return
	}
	env := auditEnvelope{Type: "result", Result: r}
	if err := s.enqueueEnvelope(&env); err != nil {
		log.Printf("[exec] redis enqueue result failed, fallback sync: %v", err)
		s.persistSync(r)
	}
}

func (s *Service) enqueueEnvelope(env *auditEnvelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	return s.rdb.LPush(ctx, s.redisQueue, string(raw)).Err()
}

func (s *Service) auditConsumerLoop() {
	for {
		select {
		case <-s.auditCancel:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := s.rdb.BRPop(ctx, 2*time.Second, s.redisQueue).Result()
		cancel()
		if err != nil {
			if err == redis.Nil || err == context.DeadlineExceeded {
				continue
			}
			log.Printf("[exec] redis consume error: %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if len(out) != 2 {
			continue
		}
		var env auditEnvelope
		if err := json.Unmarshal([]byte(out[1]), &env); err != nil {
			log.Printf("[exec] redis payload decode error: %v", err)
			continue
		}
		switch env.Type {
		case "decision":
			if env.Decision != nil {
				s.persistDecisionSync(env.Decision)
			}
		case "result":
			if env.Result != nil {
				s.persistSync(env.Result)
			}
		default:
			log.Printf("[exec] unknown audit envelope type=%s", env.Type)
		}
	}
}

func (s *Service) tryAcquireRedisLock(instrumentID string) (bool, error) {
	if s.rdb == nil {
		return false, fmt.Errorf("redis not enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	key := "mm:lock:" + instrumentID
	ok, err := s.rdb.SetNX(ctx, key, s.Address().Hex(), s.redisLockTTL).Result()
	return ok, err
}

func (s *Service) releaseRedisLock(instrumentID string) {
	if s.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	_ = s.rdb.Del(ctx, "mm:lock:"+instrumentID).Err()
}
