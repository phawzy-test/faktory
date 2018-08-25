package storage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"regexp"

	faktory "github.com/contribsys/faktory/client"
	"github.com/contribsys/faktory/util"
	"github.com/go-redis/redis"
)

type redisStore struct {
	Name      string
	mu        sync.Mutex
	queueSet  map[string]*redisQueue
	scheduled *redisSorted
	retries   *redisSorted
	dead      *redisSorted
	working   *redisSorted

	client *redis.Client
	DB     int
}

var (
	opens      = 0
	instances  = map[string]*exec.Cmd{}
	redisMutex = sync.Mutex{}
)

func MustBootRedis() {
	datapath := os.Getenv("FAKTORY_REDIS_PATH")
	if datapath == "" {
		datapath = "/var/lib/faktory/default"
	}
	sock := os.Getenv("FAKTORY_REDIS_SOCK")
	if sock == "" {
		sock = "/var/run/faktory-default.sock"
	}
	BootRedis(datapath, sock)
}

func BootRedis(path string, sock string) {
	util.LogInfo = true
	util.LogDebug = true

	redisMutex.Lock()
	defer redisMutex.Unlock()
	if _, ok := instances[sock]; ok {
		return
	}
	util.Infof("Initializing redis storage at %s, socket %s", path, sock)

	err := os.MkdirAll(path, os.ModeDir|0755)
	if err != nil {
		panic(err)
	}

	client := redis.NewClient(&redis.Options{
		Network: "unix",
		Addr:    sock,
	})

	_, err = client.Ping().Result()
	if err != nil {
		util.Debugf("Redis not alive, booting... -- %s", err)

		conffilename := "/tmp/redis.conf"
		if _, err := os.Stat(conffilename); err != nil {
			if err != nil && os.IsNotExist(err) {
				err := ioutil.WriteFile("/tmp/redis.conf", []byte(fmt.Sprintf(redisconf, faktory.Version)), 0444)
				if err != nil {
					panic(err)
				}
			} else {
				panic(err)
			}
		}

		loglevel := "notice"
		if util.LogDebug {
			loglevel = "verbose"
		}
		arguments := []string{
			"/usr/local/bin/redis-server",
			conffilename,
			"--unixsocket",
			sock,
			"--loglevel",
			loglevel,
			"--dir",
			path,
		}

		cmd := exec.Command(arguments[0], arguments[1:]...)
		instances[sock] = cmd
		err := cmd.Start()
		if err != nil {
			panic(err)
		}

		// wait a few seconds for Redis to start
		start := time.Now()
		for i := 0; i < 1000; i++ {
			conn, err := net.Dial("unix", sock)
			if err == nil {
				conn.Close()
				break
			}

			time.Sleep(10 * time.Millisecond)
		}
		done := time.Now()
		util.Debugf("Redis booted in %s", done.Sub(start))
	}

	_, err = client.Ping().Result()
	if err != nil {
		panic(err)
	}

	infos, err := client.Info().Result()
	if err != nil {
		panic(err)
	}
	version := "Unknown"
	scanner := bufio.NewScanner(bytes.NewBufferString(infos))
	for scanner.Scan() {
		txt := scanner.Text()
		if strings.Index(txt, "redis_version") != -1 {
			version = strings.Split(txt, ":")[1]
		}
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}

	util.Infof("Running Redis v%s", version)
	err = client.Close()
	if err != nil {
		panic(err)
	}
}

func OpenRedis(sock string) (Store, error) {
	util.LogInfo = true

	if _, ok := instances[sock]; !ok {
		return nil, errors.New("redis not booted, cannot start")
	}

	db := 0
	rs := &redisStore{
		Name:     sock,
		DB:       db,
		mu:       sync.Mutex{},
		queueSet: map[string]*redisQueue{},
	}
	rs.initSorted()

	rs.client = redis.NewClient(&redis.Options{
		Network: "unix",
		Addr:    sock,
		DB:      db,
	})
	_, err := rs.client.Ping().Result()
	if err != nil {
		return nil, err
	}
	return rs, nil
}

func (store *redisStore) Stats() map[string]string {
	return map[string]string{
		"stats": store.client.Info().String(),
		"name":  store.Name,
	}
}

// queues are iterated in sorted, lexigraphical order
func (store *redisStore) EachQueue(x func(Queue)) {
	for _, k := range store.queueSet {
		x(k)
	}
}

func (store *redisStore) Compact() error {
	return nil
}

func (store *redisStore) Flush() error {
	return store.client.FlushDB().Err()
}

var (
	ValidQueueName = regexp.MustCompile(`\A[a-zA-Z0-9._-]+\z`)
)

func (store *redisStore) GetQueue(name string) (Queue, error) {
	if name == "" {
		return nil, fmt.Errorf("queue name cannot be blank")
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	q, ok := store.queueSet[name]
	if ok {
		return q, nil
	}

	if !ValidQueueName.MatchString(name) {
		return nil, fmt.Errorf("queue names must match %v", ValidQueueName)
	}

	q = store.NewQueue(name)
	err := q.init()
	if err != nil {
		return nil, err
	}
	store.queueSet[name] = q
	return q, nil
}

func (store *redisStore) Close() error {
	util.Debug("Stopping storage")
	store.mu.Lock()
	defer store.mu.Unlock()

	opens -= 1
	return nil
}

func (store *redisStore) Redis() *redis.Client {
	return store.client
}

func StopRedis(sock string) error {
	redisMutex.Lock()
	defer redisMutex.Unlock()

	cmd, ok := instances[sock]
	if !ok {
		return errors.New("No such redis instance " + sock)
	}

	util.Infof("Shutting down Redis PID %d", cmd.Process.Pid)
	p := cmd.Process
	err := p.Signal(syscall.SIGTERM)
	if err != nil {
		return err
	}
	_, err = p.Wait()
	if err != nil {
		return err
	}
	delete(instances, sock)

	return nil
}

func (store *redisStore) Retries() SortedSet {
	return store.retries
}

func (store *redisStore) Scheduled() SortedSet {
	return store.scheduled
}

func (store *redisStore) Working() SortedSet {
	return store.working
}

func (store *redisStore) Dead() SortedSet {
	return store.dead
}

func (store *redisStore) EnqueueAll(sset SortedSet) error {
	return errors.New("EnqueueAll not implemented")
}

func (store *redisStore) EnqueueFrom(sset SortedSet, key []byte) error {
	return errors.New("EnqueueFrom not implemented")
}

const (
	redisconf = `
# DO NOT EDIT
# Created by Faktory %s
bind 127.0.0.1 ::1
protected-mode yes
port 0
tcp-backlog 511

unixsocket /tmp/faktory-redis.sock
unixsocketperm 700
timeout 0
tcp-keepalive 30

daemonize no
supervised no

# Specify the server verbosity level.
# This can be one of:
# debug (a lot of information, useful for development/testing)
# verbose (many rarely useful info, but not a mess like the debug level)
# notice (moderately verbose, what you want in production probably)
# warning (only very important / critical messages are logged)
loglevel notice
logfile /tmp/redis.log

databases 16

save 900 1
save 300 10
save 60 100
stop-writes-on-bgsave-error yes
rdbcompression yes
rdbchecksum yes
dbfilename faktory.rdb
dir /usr/local/var/db/redis/

slave-serve-stale-data yes
slave-read-only yes
slave-priority 100

repl-diskless-sync no
repl-diskless-sync-delay 5
repl-disable-tcp-nodelay no

lua-time-limit 5000

slowlog-log-slower-than 10000
slowlog-max-len 128
	`
)