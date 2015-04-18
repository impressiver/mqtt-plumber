package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	MQTT "git.eclipse.org/gitroot/paho/org.eclipse.paho.mqtt.golang.git"
	influx "github.com/influxdb/influxdb/client"
)

// MQTT $SYS date form
const date_form = "0000-00-00 00:00:00-0000"

// Matcher for the above date form
var re_date = regexp.MustCompile(`^[\d]{4}-[\d]{2}-[\d]{2}\s[\d]{2}:[\d]{2}:[\d]{2}-[\d]{4}$`)

// Match numeric values (int, float, etc)
var re_numeric = regexp.MustCompile(`^[0-9\.]+$`)

// Match json
var re_json = regexp.MustCompile(`^\{.*\}$`)

//
// CLI flags
//

var opt_verbose *bool

//
// Database
//

func persist(topic string, p []byte) {
	// Convert json to string:obj map
	var data map[string]interface{}
	if err := json.Unmarshal(p, &data); err != nil {
		panic(err)
	}

	// Split map into key/value arrays
	var keys []string
	var values []interface{}
	for key, value := range data {
		keys = append(keys, key)
		values = append(values, value)
	}

	// Create series for the topic, with keys for columns and values for points
	series := &influx.Series{
		Name:    topic,
		Columns: keys,
		Points: [][]interface{}{
			values,
		},
	}

	// Create db connection
	c, err := influx.NewClient(&influx.ClientConfig{
		Username: "plumber",
		Password: "plumber",
		Database: "mqtt_plumber",
	})
	if err != nil {
		panic(err)
	}

	// Write to db or die trying
	if err := c.WriteSeries([]*influx.Series{series}); err != nil {
		panic(err)
	}

	if *opt_verbose {
		fmt.Printf("Persisted: %s", series)
	}
}

//
// Message parsing
//

func parse(payload []byte) []byte {
	// Unmarshal payload by parsing the string value, wrapping in json, and
	// converting to bytes, theb using the json unmarshaler to figure out what the
	// correct numeric value types should be

	var json_payload []byte
	var matched string
	if re_json.Match(payload) {
		matched = "json"
		json_payload = payload
	} else if re_numeric.Match(payload) {
		matched = "numeric"
		// Let json unmarshaler figure out what type of numeric
		json_payload = []byte(fmt.Sprintf("{\"value\": %s}", payload))
	} else if re_date.Match(payload) {
		matched = "date"
		// Reformat dates as ISO-8601 (w/o nanos)
		t, _ := time.Parse(date_form, string(payload[:]))
		json_payload = []byte(fmt.Sprintf("{\"value\": \"%s\"}", t.Format(time.RFC3339)))
	} else {
		matched = "string"
		// Quote the value as a string
		json_payload = []byte(fmt.Sprintf("{\"value\": \"%s\"}", payload))
	}

	if *opt_verbose {
		fmt.Printf("(%s) %s\n", matched, json_payload)
	}

	return json_payload
}

//
// Message handlers
//

func onSysMessageReceived(mqtt *MQTT.MqttClient, message MQTT.Message) {
	fmt.Printf("Received message on $SYS topic: %s\n", message.Topic())
	fmt.Printf("%s\n", message.Payload())

	// Save the processed message
	persist(message.Topic(), parse(message.Payload()))
}

func onTopicMessageReceived(mqtt *MQTT.MqttClient, message MQTT.Message) {
	fmt.Printf("Received message on subscribed topic: %s\n", message.Topic())
	fmt.Printf("%s\n", message.Payload())

	// Save the processed message
	persist(message.Topic(), parse(message.Payload()))
}

func onAnyMessageReceived(mqtt *MQTT.MqttClient, message MQTT.Message) {
	fmt.Printf("Received message on unsubscribed topic: %s\n", message.Topic())
	fmt.Printf("%s\n", parse(message.Payload()))
}

//
// Main
//

func main() {
	stdin := bufio.NewReader(os.Stdin)
	rand.Seed(time.Now().Unix())

	// Config
	broker := flag.String("broker", "tcp://mashtun:1883", "The MQTT server uri")
	client := flag.String("client", "plumber-"+strconv.Itoa(rand.Intn(1000)), "The client id")
	watch := flag.String("watch", "broadcast/#", "A comma-separated list of topics")
	sys := flag.Bool("sys", false, "Persist $SYS status messages")
	publish := flag.String("publish", "broadcast/client/{client}", "Default publish topic")
	prefix := flag.String("prefix", "", "Base topic hierarchy (namespace) prepended to subscriptions")
	qos := flag.Int("qos", 0, "QoS level for published messages")
	clean := flag.Bool("clean", true, "Start with a clean session")
	verbose := flag.Bool("verbose", false, "Increased logging")
	flag.Parse()

	// Set global flags
	opt_verbose = verbose

	// Default publish topic (for sending stdin to MQTT)
	default_pub_topic := strings.Replace(*publish, "{client}", *client, -1)

	// Init MQTT options
	opts := MQTT.NewClientOptions().AddBroker(*broker).SetClientId(*client).SetCleanSession(*clean)
	if *verbose {
		opts.SetDefaultPublishHandler(onAnyMessageReceived)
	}

	// Create client and connect
	mqtt := MQTT.NewClient(opts)
	_, err := mqtt.Start()

	// Boo
	if err != nil {
		panic(err)
	}

	// Successfully connected
	fmt.Printf("Connected to %s as %s\n\n", *broker, *client)

	// Subscribe to $SYS topic
	if *sys {
		fmt.Println("Subscribing to $SYS")
		sys_filter, _ := MQTT.NewTopicFilter("$SYS/#", 1)
		mqtt.StartSubscription(onSysMessageReceived, sys_filter)
	}

	// Split comma-separated watch list into array of topics
	topiclist := strings.Split(*watch, ",")
	for i := range topiclist {
		topic := strings.TrimSpace(topiclist[i])

		// Add prefix, if configured
		if len(*prefix) > 0 {
			strings.Join([]string{*prefix, strings.TrimSpace(topiclist[i])}, "/")
		}

		// Subscribe to topic
		fmt.Println("Subscribing to " + topic)
		topic_filter, _ := MQTT.NewTopicFilter(topic, 1)
		mqtt.StartSubscription(onTopicMessageReceived, topic_filter)
	}

	// Watch stdin and publish input to MQTT
	for {
		fmt.Print("\n> ")

		// Read a line of input
		in, err := stdin.ReadString('\n')

		// Borked
		if err == io.EOF {
			os.Exit(0)
		}

		// Split input on first space: {the/pub/topic} {message payload with spaces}
		var pub_topic, message = func(parts []string) (string, string) {
			if len(parts) == 2 {
				return strings.Replace(parts[0], "{client}", *client, -1), parts[1]
			} else {
				return default_pub_topic, parts[0]
			}
		}(strings.SplitN(strings.TrimSpace(in), " ", 2))

		// Don't publish empty messages
		if len(message) == 0 {
			continue
		}

		// Publish to MQTT
		fmt.Printf("Publishing to %s: %s\n", pub_topic, message)
		res := mqtt.Publish(MQTT.QoS(*qos), pub_topic, []byte(message))
		<-res
	}
}
