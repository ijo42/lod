package cache

import (
	"context"
	"crypto/tls"
	"sync"

	"github.com/allegro/bigcache/v3"
	"github.com/go-redis/redis/v8"
	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/dechristopher/lod/config"
	"github.com/dechristopher/lod/env"
	"github.com/dechristopher/lod/str"
	"github.com/dechristopher/lod/util"
)

var Subsystem = "cache"

// Caches configured for this instance
var Caches CachesMap = make(map[string]*Cache)

// cacheLock is used internally to prevent redundant
// concurrent initialization of cache instances
var cacheLock *sync.Mutex

// CachesMap is an alias type for the map of proxy name to its cache
type CachesMap map[string]*Cache

// Cache is a wrapper struct that operates a dual cache against the in-memory
// cache and Redis as a backing cache
type Cache struct {
	internal *bigcache.BigCache // pointer to internal cache instance
	external *redis.Client      // pointer to external Redis cache
	Proxy    *config.Proxy      // copy of the proxy configuration
	Metrics  *Metrics           // metrics container instance
}

// Metrics for the cache instance
type Metrics struct {
	CacheHits   prometheus.Counter     // cache hits
	CacheMisses prometheus.Counter     // cache misses
	HitRate     prometheus.CounterFunc // cache hit rate
}

// OneMB represents one megabyte worth of bytes
const OneMB = 1024 * 1024

// Get a cache instance by name
func Get(name string) *Cache {
	if Caches[name] == nil {
		return buildInstance(name)
	}

	return Caches[name]
}

// buildInstance will build a cache instance by name
func buildInstance(name string) *Cache {
	if cacheLock == nil {
		cacheLock = &sync.Mutex{}
	}

	cacheLock.Lock()
	defer cacheLock.Unlock()
	// find and populate a new cache instance for the given name
	for _, proxy := range config.Get().Proxies {
		if proxy.Name == name {
			var internal *bigcache.BigCache
			var external *redis.Client
			var err error

			if proxy.Cache.MemEnabled {
				internal, err = initInternal(proxy)
				if err != nil {
					util.Error(str.CCache, str.ECacheCreate, err.Error())
					return nil
				}
			}

			if proxy.Cache.RedisEnabled {
				external, err = initExternal(proxy)
				if err != nil {
					util.Error(str.CCache, str.ECacheCreate, err.Error())
					return nil
				}
			}

			// initialize metrics for this cache instance
			metrics := initMetrics(proxy)

			util.DebugFlag("cache", str.CCache, str.DCacheUp, name)

			Caches[name] = &Cache{
				internal: internal,
				external: external,
				Proxy:    &proxy,
				Metrics:  metrics,
			}

			return Caches[name]
		}
	}
	// if this happens, there's an edge case somewhere
	util.Error(str.CCache, str.ECacheName, name)
	return nil
}

// initInternal initializes an in-memory cache instance from proxy configuration
func initInternal(proxy config.Proxy) (*bigcache.BigCache, error) {
	conf := bigcache.DefaultConfig(proxy.Cache.MemTTLDuration)
	conf.StatsEnabled = !env.IsProd()
	conf.MaxEntrySize = 1024 * 10 // 100KB
	conf.HardMaxCacheSize = OneMB * proxy.Cache.MemCap

	return bigcache.NewBigCache(conf)
}

// initExternal initializes an external cache instance from proxy configuration
func initExternal(proxy config.Proxy) (*redis.Client, error) {
	opts, err := redis.ParseURL(proxy.Cache.RedisURL)
	if err != nil {
		util.Error(str.CCache, str.ECacheCreate, err.Error())
		return nil, err
	}

	if proxy.Cache.RedisTLS {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	external := redis.NewClient(opts)

	_, err = external.Ping(context.Background()).Result()

	return external, err
}

// initMetrics for the given proxy configuration
func initMetrics(proxy config.Proxy) *Metrics {
	cacheHits := promauto.NewCounter(prometheus.CounterOpts{
		Namespace: config.Namespace,
		Subsystem: Subsystem,
		Name:      "hit_total",
		ConstLabels: map[string]string{
			"proxy": proxy.Name,
		},
		Help: "The total number of cache hits",
	})

	cacheMisses := promauto.NewCounter(prometheus.CounterOpts{
		Namespace: config.Namespace,
		Subsystem: Subsystem,
		Name:      "miss_total",
		ConstLabels: map[string]string{
			"proxy": proxy.Name,
		},
		Help: "The total number of cache misses",
	})

	hitRate := promauto.NewCounterFunc(prometheus.CounterOpts{
		Namespace: config.Namespace,
		Subsystem: Subsystem,
		Name:      "hit_rate",
		ConstLabels: map[string]string{
			"proxy": proxy.Name,
		},
		Help: "The rate of hits to misses",
	}, func() float64 {
		hits := util.GetMetricValue(cacheHits)
		misses := util.GetMetricValue(cacheMisses)
		return hits / (hits + misses)
	})

	return &Metrics{
		CacheHits:   cacheHits,
		CacheMisses: cacheMisses,
		HitRate:     hitRate,
	}
}

// Fetch will attempt to grab a tile by key from any of the cache layers,
// populating higher layers of the cache if found.
func (c *Cache) Fetch(key string, ctx *fiber.Ctx) *TilePacket {
	var cachedTile []byte
	var err error
	var hit string

	// fetch from in-memory cache if enabled
	if c.Proxy.Cache.MemEnabled {
		cachedTile, err = c.internal.Get(key)
		if err != nil {
			if err == bigcache.ErrEntryNotFound {
				util.DebugFlag("cache", str.CCache, str.DCacheMiss, key)
			} else {
				util.Error(str.CCache, str.ECacheFetch, key, err.Error())
				return nil
			}
		}

		hit = " :hit-i"
	}

	if cachedTile == nil && c.Proxy.Cache.RedisEnabled {
		// try fetching from redis if not present in internal cache
		redisTile := c.external.Get(context.Background(), key)
		if redisTile.Err() != nil {
			if redisTile.Err() == redis.Nil {
				// exit early if we don't have anything cached at any level
				c.Metrics.CacheMisses.Inc()
				util.DebugFlag("cache", str.CCache, str.DCacheMissExt, key)
				return nil
			}
			util.Error(str.CCache, str.ECacheFetch, key, err.Error())
			return nil
		}

		// squeeze out the bytes from the redis response
		cachedTile, err = redisTile.Bytes()
		if err != nil {
			util.Error(str.CCache, str.ECacheFetch, key, err.Error())
			return nil
		}

		hit = " :hit-e"

		// if TTL set, extend Redis TTL when we fetch a tile to prevent
		// key expiry for tiles that are fetched periodically
		if c.Proxy.Cache.RedisTTLDuration > 0 {
			go c.external.Expire(context.Background(), key, c.Proxy.Cache.RedisTTLDuration)
		}
	}

	if cachedTile == nil {
		// exit if we don't have anything cached at any level
		c.Metrics.CacheMisses.Inc()
		util.DebugFlag("cache", str.CCache, str.DCacheMissExt, key)
		return nil
	}

	ctx.Locals("lod-cache", hit)
	c.Metrics.CacheHits.Inc()

	// wrap bytes in TilePacket container
	tile := TilePacket(cachedTile)
	// ensure we've got valid tile protobuf bytes
	if len(tile) == 0 || !tile.Validate() {
		// exit early and wipe cache if we cached a bad value
		util.DebugFlag("cache", str.CCache, str.DCacheFail, key)
		err = c.Invalidate(key)
		if err != nil {
			util.Error(str.CCache, str.ECacheDelete, key, err.Error())
		}
		return nil
	}

	util.DebugFlag("cache", str.CCache, str.DCacheHit, key, len(tile))

	// extend internal cache TTL (keeping entry alive) by resetting the entry
	// this also sets internal cache entries if we find a tile in redis but not internally
	// TODO investigate alternative methods of preventing entry death
	go c.Set(key, cachedTile, true)

	return &tile
}

// EncodeSet will encode tile data into a TilePacket and then set the cache
// entry to the specified key
func (c *Cache) EncodeSet(key string, tileData []byte, headers map[string]string) {
	packet := c.Encode(key, tileData, headers)
	c.Set(key, packet)
}

// Set the tile in all cache levels with the configured TTLs
func (c *Cache) Set(key string, tile TilePacket, internalOnly ...bool) {
	util.DebugFlag("cache", str.CCache, str.DCacheSet, key, len(tile))

	// set in external cache if enabled and allowed
	if (len(internalOnly) == 0 || !internalOnly[0]) && c.Proxy.Cache.RedisEnabled {
		go func() {
			status := c.external.Set(context.Background(), key,
				tile.Raw(), c.Proxy.Cache.RedisTTLDuration)
			if status.Err() != nil {
				util.Error(str.CCache, str.ECacheSet, key, status.Err())
			}
		}()
	}

	// set in the in-memory cache if enabled
	if c.Proxy.Cache.MemEnabled {
		err := c.internal.Set(key, tile)
		if err != nil {
			util.Error(str.CCache, str.ECacheSet, key, err.Error())
		}
	}
}

// Invalidate a tile by key from all cache levels
func (c *Cache) Invalidate(key string) error {
	// invalidate from in-memory cache if enabled
	if c.Proxy.Cache.MemEnabled {
		err := c.internal.Delete(key)
		if err != nil && err != bigcache.ErrEntryNotFound {
			return err
		}
	}

	if c.Proxy.Cache.RedisEnabled {
		status := c.external.Del(context.Background(), key)
		if status.Err() != nil {
			return status.Err()
		}
	}

	return nil
}

// Flush the internal bigcache instance
func (c *Cache) Flush() error {
	if c.Proxy.Cache.MemEnabled {
		return c.internal.Reset()
	}
	return nil
}

func (c *Cache) Stats() bigcache.Stats {
	if c.Proxy.Cache.MemEnabled {
		return c.internal.Stats()
	}
	return bigcache.Stats{}
}
