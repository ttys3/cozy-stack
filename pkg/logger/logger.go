package logger

import (
	"context"
	"io/ioutil"
	"strings"
	"sync"
	"time"

	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/go-redis/redis/v8"
	"github.com/sirupsen/logrus"
)

const (
	debugRedisAddChannel = "add:log-debug"
	debugRedisRmvChannel = "rmv:log-debug"
	debugRedisPrefix     = "debug:"
)

var opts Options
var loggers = make(map[string]domainEntry)
var loggersMu sync.RWMutex

// Options contains the configuration values of the logger system
type Options struct {
	Syslog bool
	Level  string
	Redis  redis.UniversalClient
}

type domainEntry struct {
	log       *logrus.Logger
	expiredAt *time.Time
}

func (entry *domainEntry) Expired() bool {
	if entry.expiredAt == nil {
		return false
	}
	return entry.expiredAt.Before(time.Now())
}

// Init initializes the logger module with the specified options.
func Init(opt Options) error {
	level := opt.Level
	if level == "" {
		level = "info"
	}
	logLevel, err := logrus.ParseLevel(level)
	if err != nil {
		return err
	}
	logrus.SetLevel(logLevel)
	if opt.Syslog {
		hook, err := syslogHook()
		if err != nil {
			return err
		}
		logrus.AddHook(hook)
		logrus.SetOutput(ioutil.Discard)
	} else if build.IsDevRelease() && logLevel == logrus.DebugLevel {
		formatter := logrus.StandardLogger().Formatter.(*logrus.TextFormatter)
		formatter.TimestampFormat = time.RFC3339Nano
	}
	if cli := opt.Redis; cli != nil {
		ctx := context.Background()
		go subscribeLoggersDebug(ctx, cli)
		go loadDebug(ctx, cli)
	}
	opts = opt
	return nil
}

// Clone clones a logrus.Logger struct.
func Clone(in *logrus.Logger) *logrus.Logger {
	out := &logrus.Logger{
		Out:       in.Out,
		Hooks:     make(logrus.LevelHooks),
		Formatter: in.Formatter,
		Level:     in.Level,
	}
	for k, v := range in.Hooks {
		out.Hooks[k] = v
	}
	return out
}

// AddDebugDomain adds the specified domain to the debug list.
func AddDebugDomain(domain string, ttl time.Duration) error {
	if cli := opts.Redis; cli != nil {
		ctx := context.Background()
		return publishDebug(ctx, cli, debugRedisAddChannel, domain, ttl)
	}
	addDebugDomain(domain, ttl)
	return nil
}

// RemoveDebugDomain removes the specified domain from the debug list.
func RemoveDebugDomain(domain string) error {
	if cli := opts.Redis; cli != nil {
		ctx := context.Background()
		return publishDebug(ctx, cli, debugRedisRmvChannel, domain, 0)
	}
	removeDebugDomain(domain)
	return nil
}

// WithNamespace returns a logger with the specified nspace field.
func WithNamespace(nspace string) *logrus.Entry {
	return logrus.WithField("nspace", nspace)
}

// WithDomain returns a logger with the specified domain field.
func WithDomain(domain string) *logrus.Entry {
	loggersMu.RLock()
	entry, ok := loggers[domain]
	loggersMu.RUnlock()
	if ok {
		if !entry.Expired() {
			return entry.log.WithField("domain", domain)
		}
		removeDebugDomain(domain)
	}
	return logrus.WithField("domain", domain)
}

func addDebugDomain(domain string, ttl time.Duration) {
	loggersMu.Lock()
	defer loggersMu.Unlock()
	_, ok := loggers[domain]
	if ok {
		return
	}
	logger := logrus.New()
	logger.Level = logrus.DebugLevel
	if opts.Syslog {
		hook, err := syslogHook()
		if err == nil {
			logger.Hooks.Add(hook)
			logger.Out = ioutil.Discard
		}
	}
	expiredAt := time.Now().Add(ttl)
	loggers[domain] = domainEntry{logger, &expiredAt}
}

func removeDebugDomain(domain string) {
	loggersMu.Lock()
	defer loggersMu.Unlock()
	delete(loggers, domain)
}

func subscribeLoggersDebug(ctx context.Context, cli redis.UniversalClient) {
	sub := cli.Subscribe(ctx, debugRedisAddChannel, debugRedisRmvChannel)
	for msg := range sub.Channel() {
		parts := strings.Split(msg.Payload, "/")
		domain := parts[0]
		switch msg.Channel {
		case debugRedisAddChannel:
			var ttl time.Duration
			if len(parts) >= 2 {
				ttl, _ = time.ParseDuration(parts[1])
			}
			addDebugDomain(domain, ttl)
		case debugRedisRmvChannel:
			removeDebugDomain(domain)
		}
	}
}

func loadDebug(ctx context.Context, cli redis.UniversalClient) {
	keys, err := cli.Keys(ctx, debugRedisPrefix+"*").Result()
	if err != nil {
		return
	}
	for _, key := range keys {
		ttl, err := cli.TTL(ctx, key).Result()
		if err != nil {
			continue
		}
		domain := strings.TrimPrefix(key, debugRedisPrefix)
		addDebugDomain(domain, ttl)
	}
}

func publishDebug(ctx context.Context, cli redis.UniversalClient, channel, domain string, ttl time.Duration) error {
	err := cli.Publish(ctx, channel, domain+"/"+ttl.String()).Err()
	if err != nil {
		return err
	}
	key := debugRedisPrefix + domain
	if channel == debugRedisAddChannel {
		err = cli.Set(ctx, key, 0, ttl).Err()
	} else {
		err = cli.Del(ctx, key).Err()
	}
	return err
}

// DebugExpiration returns the expiration date for the debug mode for the
// instance logger of the given domain (or nil if the debug mode is not
// activated).
func DebugExpiration(domain string) *time.Time {
	loggersMu.RLock()
	entry, ok := loggers[domain]
	loggersMu.RUnlock()
	if !ok {
		return nil
	}
	return entry.expiredAt
}

// IsDebug returns whether or not the debug mode is activated.
func IsDebug(logger *logrus.Entry) bool {
	return logger.Logger.Level == logrus.DebugLevel
}
