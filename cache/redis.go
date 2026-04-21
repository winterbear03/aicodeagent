package cache

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"agent/database"
	"agent/models"

	"github.com/redis/go-redis/v9"
)

var Rdb *redis.Client
var ctx = context.Background()
var localCache sync.Map

func InitRedis() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	Rdb = redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})
	if err := Rdb.Ping(ctx).Err(); err != nil {
		panic("Redis connection failed: " + err.Error())
	}
	log.Println("✅ Redis connected")
}

func GetTask(id string) (models.Task, error) {
	if val, ok := localCache.Load(id); ok {
		return val.(models.Task), nil
	}
	data, err := Rdb.Get(ctx, "task:"+id).Bytes()
	if err == nil {
		var task models.Task
		json.Unmarshal(data, &task)
		localCache.Store(id, task)
		return task, nil
	}
	var task models.Task
	err = database.DB.First(&task, "id = ?", id).Error
	if err != nil {
		return task, err
	}
	SetTaskCache(task)
	return task, nil
}

func SetTaskCache(task models.Task) {
	localCache.Store(task.ID, task)
	data, _ := json.Marshal(task)
	Rdb.Set(ctx, "task:"+task.ID, data, 24*time.Hour)
}

func DeleteTaskCache(id string) {
	localCache.Delete(id)
	Rdb.Del(ctx, "task:"+id)
}
