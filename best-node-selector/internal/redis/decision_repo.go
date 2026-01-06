package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"best-node-selector/internal/models"

	goredis "github.com/redis/go-redis/v9"
)

type DecisionRepository struct {
	rdb *goredis.Client
	ttl time.Duration
}

func NewDecisionRepository(addr string, ttl time.Duration) *DecisionRepository {
	rdb := goredis.NewClient(&goredis.Options{
		Addr: addr,
	})

	return &DecisionRepository{
		rdb: rdb,
		ttl: ttl,
	}
}

func decisionKey(namespace, service string) string {
	return fmt.Sprintf("scheduler:decision:%s:%s", namespace, service)
}

// Save implements app.DecisionReader / scorer.DecisionWriter
func (r *DecisionRepository) Save(
	ctx context.Context,
	d *models.Decision,
) error {

	data, err := json.Marshal(d)
	if err != nil {
		return err
	}

	key := decisionKey(d.Namespace, d.Service)
	return r.rdb.Set(ctx, key, data, r.ttl).Err()
}

// Get implements app.DecisionReader
func (r *DecisionRepository) Get(
	ctx context.Context,
	namespace, service string,
) (*models.Decision, error) {

	key := decisionKey(namespace, service)

	val, err := r.rdb.Get(ctx, key).Result()
	if err == goredis.Nil {
		return nil, fmt.Errorf("decision not found")
	}
	if err != nil {
		return nil, err
	}

	var d models.Decision
	if err := json.Unmarshal([]byte(val), &d); err != nil {
		return nil, err
	}

	return &d, nil
}

// List returns all decisions for a namespace
func (r *DecisionRepository) List(
	ctx context.Context,
	namespace string,
) ([]*models.Decision, error) {

	pattern := fmt.Sprintf("scheduler:decision:%s:*", namespace)

	var cursor uint64
	out := make([]*models.Decision, 0)

	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, pattern, 50).Result()
		if err != nil {
			return nil, err
		}

		for _, key := range keys {
			val, err := r.rdb.Get(ctx, key).Result()
			if err != nil {
				continue // skip expired / race
			}

			var d models.Decision
			if err := json.Unmarshal([]byte(val), &d); err != nil {
				continue
			}

			out = append(out, &d)
		}

		cursor = next
		if cursor == 0 {
			break
		}
	}

	return out, nil
}
