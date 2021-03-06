package main

import (
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/garyburd/redigo/redis"
	"github.com/juju/loggo"
	"github.com/ninjablocks/sphere-go-state-service/health"
	"github.com/ninjablocks/sphere-go-state-service/stats"
	"github.com/rcrowley/go-metrics"
	"github.com/rcrowley/go-metrics/librato"
	"github.com/streadway/amqp"
	"github.com/wolfeidau/punter"
)

var (
	debug       = kingpin.Flag("debug", "Enable debug mode.").OverrideDefaultFromEnvar("DEBUG").Bool()
	workers     = kingpin.Flag("workers", "Configure the number of workers.").Default("4").OverrideDefaultFromEnvar("WORKERS").Int()
	redisURL    = kingpin.Flag("redis", "REDIS url.").Default("redis://localhost:6379").OverrideDefaultFromEnvar("REDIS_URL").String()
	rabbitmqURL = kingpin.Flag("rabbitmq", "rabbitmq url.").Default("amqp://guest:guest@localhost:5672").OverrideDefaultFromEnvar("RABBIT_URL").String()
	libratoKey  = kingpin.Flag("libratoKey", "Librato API key.").OverrideDefaultFromEnvar("LIBRATO_KEY").String()
	statusAddr  = kingpin.Flag("statusAddr", "Address to assign to the status listener.").OverrideDefaultFromEnvar("PORT").Default(":6100").String()

	log = loggo.GetLogger("state-service")

	routingKey = "*.$cloud.device.*.channel.*.event.state"
	userRegex  = regexp.MustCompile(`^(?P<user_id>[a-zA-Z0-9-_]+).\$cloud.device.(?P<device_id>\w+).channel.(?P<channel_id>[a-zA-Z0-9-_]+).event.state$`)

	hostname = "unknown"
)

func main() {

	kingpin.Version(Version)
	kingpin.Parse()

	// apply flags
	if *debug {
		loggo.GetLogger("").SetLogLevel(loggo.DEBUG)
	} else {
		loggo.GetLogger("").SetLogLevel(loggo.INFO)
	}

	rurl, err := url.Parse(*redisURL)

	if err != nil {
		panic(err)
	}

	hostname, err = os.Hostname()

	if err != nil {
		panic(err)
	}

	log.Infof("hostname %+v", hostname)

	c := metrics.NewCounter()
	metrics.Register("timeseries.messages_processed", c)

	t := metrics.NewTimer()
	metrics.Register("timeseries.messages_processed_time", t)

	//	go metrics.Log(metrics.DefaultRegistry, 30e9, glog.New(os.Stderr, "metrics: ", glog.Lmicroseconds))

	startLibrato()
	stats.StartRuntimeMetricsJob("prod")

	ss := &stateStore{
		pool: newPool(rurl.Host),
		c:    c,
		t:    t,
	}

	consumers := []*punter.Consumer{}

	conf := &punter.Config{
		AmqpURI:      *rabbitmqURL,
		Exchange:     "amq.topic",
		ExchangeType: "topic",
		QueueName:    "stateservice",
		Key:          routingKey,
		MessageTTL:   int32(600000), // How long to retain messages in the queue (10 minutes)
		Durable:      false,         // Queue durable?
	}

	for i := 0; i < *workers; i++ {

		consumer, err := punter.NewConsumer(conf, fmt.Sprintf("stateservice-consumer-%s", hostname), ss.stateHandler)
		if err != nil {
			panic(err)
		}

		consumers = append(consumers, consumer)
	}

	health.StartHttpListener(*statusAddr, BuildInfo)

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, os.Interrupt, os.Kill)

	// Block until a signal is received.
	s := <-sc
	log.Warningf("Got signal: %v", s)

	log.Warningf("shutting down consumer")

	for n, consumer := range consumers {
		log.Infof("shutting down consumer %d", n)
		if err := consumer.Shutdown(); err != nil {
			log.Infof("error during shutdown: %s", err)
		}
	}

}

func newPool(server string) *redis.Pool {
	return &redis.Pool{
		MaxIdle:     3,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial("tcp", server)
			if err != nil {
				return nil, err
			}
			return c, err
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
}

func startLibrato() {

	if *libratoKey == "" {
		log.Warningf("skipping librato job as no key is set.")
		return
	}

	go librato.Librato(metrics.DefaultRegistry,
		30e9, // interval
		"services@ninjablocks.com", // account owner email address
		*libratoKey,                // Librato API token
		hostname,                   // source
		[]float64{0.95},            // precentiles to send
		time.Millisecond,           // time unit
	)
}

type stateStore struct {
	pool *redis.Pool
	c    metrics.Counter
	t    metrics.Timer
}

func (ss *stateStore) stateHandler(deliveries <-chan amqp.Delivery, done chan error) {
	for d := range deliveries {

		ss.c.Inc(1)

		start := time.Now()

		log.Debugf(
			"amqp key: %s payload: %dB delivery: [%v]",
			d.RoutingKey,
			len(d.Body),
			d.DeliveryTag,
		)

		err := ss.savePayload(d.Body, d.RoutingKey)

		if err != nil {
			log.Errorf("failed to process payload: %s", err)
		}

		d.Ack(false)
		ss.t.UpdateSince(start)
	}
	log.Debugf("handle: deliveries channel closed")
	done <- nil
}

// cache the state in redis using a key based on state:{user_id}:{device_id}:{channel_id}
func (ss *stateStore) savePayload(body []byte, routingKey string) error {

	params := getParams(routingKey)

	if params == nil {
		return fmt.Errorf("bad routing key - " + routingKey)
	}

	// state:123:b6b984190f:on-off
	key := fmt.Sprintf("state:%s:%s:%s", params["user_id"], params["device_id"], params["channel_id"])

	c := ss.pool.Get()

	n, err := c.Do("SET", key, string(body))

	if err != nil {
		return err
	}

	log.Debugf("redis key = %s n = %v", key, n)

	return nil
}

func getParams(routingKey string) map[string]string {

	matches := userRegex.FindAllStringSubmatch(routingKey, -1)

	if matches == nil {
		return nil
	}

	params := make(map[string]string)

	for i, attr := range userRegex.SubexpNames() {
		if attr == "" {
			continue
		}
		params[attr] = matches[0][i]
	}

	return params
}
