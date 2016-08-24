package statsd

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/influxdb/client/v2"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/streadway/amqp"
)

// RabbitMQParser is the top level struct for this plugin
type RabbitMQParser struct {
	RabbitmqAddress string
	QueueName       string

	conn *amqp.Connection
	ch   *amqp.Channel
	q    amqp.Queue

	sync.Mutex
}

// Description satisfies the telegraf.ServiceInput interface
func (rmq *RabbitMQParser) Description() string {
	return "RabbitMQ client with specialized parser"
}

// SampleConfig satisfies the telegraf.ServiceInput interface
func (rmq *RabbitMQParser) SampleConfig() string {
	return `
  ## Address and port for the rabbitmq server to pull from 
  rabbitmq_address = "amqp://guest:guest@localhost:5672/"
  queue_name = "task_queue"
`
}

// Gather satisfies the telegraf.ServiceInput interface
// All gathering is done in the Start function
func (rmq *RabbitMQParser) Gather(_ telegraf.Accumulator) error {
	return nil
}

// Start satisfies the telegraf.ServiceInput interface
// Yanked from "https://www.rabbitmq.com/tutorials/tutorial-two-go.html"
func (rmq *RabbitMQParser) Start(acc telegraf.Accumulator) error {

	// Create queue connection and assign it to RabbitMQParser
	conn, err := amqp.Dial(rmq.RabbitmqAddress)
	if err != nil {
		return fmt.Errorf("%v: Failed to connect to RabbitMQ", err)
	}
	rmq.conn = conn

	// Create channel and assign it to RabbitMQParser
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("%v: Failed to open a channel", err)
	}
	rmq.ch = ch

	// Declare a queue and assign it to RabbitMQParser
	q, err := ch.QueueDeclare(rmq.QueueName, true, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("%v: Failed to declare a queue", err)
	}
	rmq.q = q

	// Declare QoS on queue
	err = ch.Qos(1, 0, false)
	if err != nil {
		return fmt.Errorf("%v: failed to set Qos", err)
	}

	// Register the RabbitMQ parser as a consumer of the queue
	// And start the lister passing in the Accumulator
	msgs := rmq.registerConsumer()
	go listen(msgs, acc)

	// Log that service has started
	log.Println("Starting RabbitMQ service...")
	return nil
}

// Yanked from "https://www.rabbitmq.com/tutorials/tutorial-two-go.html"
func (rmq *RabbitMQParser) registerConsumer() <-chan amqp.Delivery {
	messages, err := rmq.ch.Consume(rmq.QueueName, "", false, false, false, false, nil)
	if err != nil {
		panic(fmt.Errorf("%v: failed establishing connection to queue", err))
	}
	return messages
}

// Iterate over messages as they are coming in
// and launch new goroutine to handle load
func listen(msgs <-chan amqp.Delivery, acc telegraf.Accumulator) {
	for d := range msgs {
		go handleMessage(d, acc)
	}
}

// handleMessage parses the incoming messages into *client.Point
// and then adds them to the Accumulator
func handleMessage(d amqp.Delivery, acc telegraf.Accumulator) {
	msg := sanitizeMsg(d)
	acc.AddFields(msg.Name(), msg.Fields(), msg.Tags(), msg.Time())
	d.Ack(false)
}

// sanitizeMsg breaks message cleanly into the different parts
// turns them into an IR and returns a point
func sanitizeMsg(msg amqp.Delivery) *client.Point {
	ir := &irMessage{}
	if strings.Contains(string(msg.Body), `"host"`) {
		text := strings.Split(string(msg.Body), "\"host\"")
		hostSplit := strings.Split(text[1], "\"clock\"")
		ir.host = hostSplit[0]
		clockSplit := strings.Split(hostSplit[1], "\"value\"")
		ir.clock = clockSplit[0]
		valueSplit := strings.Split(clockSplit[1], "\"key\"")
		ir.value = valueSplit[0]
		keySplit := strings.Split(valueSplit[1], "\"server\"")
		ir.key = keySplit[0]
		ir.server = keySplit[1]
		ir.doubleQuoted = true
	} else {
		text := strings.Split(string(msg.Body), "'host'")
		hostSplit := strings.Split(text[1], "'clock'")
		ir.host = hostSplit[0]
		clockSplit := strings.Split(hostSplit[1], "'value'")
		ir.clock = clockSplit[0]
		valueSplit := strings.Split(clockSplit[1], "'key'")
		ir.value = valueSplit[0]
		keySplit := strings.Split(valueSplit[1], "'server'")
		ir.key = keySplit[0]
		ir.server = keySplit[1]
		ir.doubleQuoted = false
	}
	return ir.message().point()
}

// Takes the intermediate representation and turns it into a message
func (ir *irMessage) message() message {
	var msg message

	// trim trailing chars from value
	ir.value = string(ir.value[2 : len(ir.value)-2])

	// trim trailing chars from key
	ir.key = string(ir.key[3 : len(ir.key)-3])

	// check what type of value is to be stored
	// "'" indicates string messages
	if strings.ContainsAny(ir.value, "'") {
		msg = ir.toStringMessage()
	} else {
		msg = ir.toFloatMessage()
	}

	return msg
}

// irMessage is an intermediate representation of the
// point as it moves through the parser
type irMessage struct {
	host         string
	clock        string
	value        string
	key          string
	server       string
	doubleQuoted bool
}

// cleans host and server names
func cleanHost(str string) string {
	c := strings.Split(str, "'")
	return c[1]
}

// takes a dirty timestamp string and turns it into time.Time
func cleanClock(str string) time.Time {
	c := string(str[2 : len(str)-2])
	i, err := strconv.ParseInt(c, 10, 64)
	if err != nil {
		panic(fmt.Errorf("%v: parsing integer", err))
	}
	return time.Unix(i, 0)
}

// irMessage -> *strMessage
func (ir *irMessage) toStringMessage() *strMessage {
	sm := &strMessage{}
	if ir.doubleQuoted {
		sm.host = cleanHost(strings.Replace(ir.host, "\"", "'", -1))
		sm.clock = cleanClock(strings.Replace(ir.clock, "\"", "'", -1))
		sm.server = cleanHost(strings.Replace(ir.host, "\"", "'", -1))
		sm.value = ir.value
		sm.key = ir.key
	} else {
		sm.host = cleanHost(ir.host)
		sm.clock = cleanClock(ir.clock)
		sm.server = cleanHost(ir.server)
		sm.value = ir.value
		sm.key = ir.key
	}
	return sm
}

// irMessage -> *floatMessage
func (ir *irMessage) toFloatMessage() *floatMessage {
	fm := &floatMessage{}
	if ir.doubleQuoted {
		fm.host = cleanHost(strings.Replace(ir.host, "\"", "'", -1))
		fm.clock = cleanClock(strings.Replace(ir.clock, "\"", "'", -1))
		fm.server = cleanHost(strings.Replace(ir.host, "\"", "'", -1))
		i, err := strconv.ParseFloat(ir.value, 64)
		if err != nil {
			panic(fmt.Errorf("%v: parsing float", err))
		}
		fm.value = i
		fm.key = ir.key
	} else {
		fm.host = cleanHost(ir.host)
		fm.clock = cleanClock(ir.clock)
		fm.server = cleanHost(ir.server)
		i, err := strconv.ParseFloat(ir.value, 64)
		if err != nil {
			panic(fmt.Errorf("%v: parsing float", err))
		}
		fm.value = i
		fm.key = ir.key
	}
	return fm
}

// This is an awful decision tree parsing, but it works...
// Need to hone with more data
func structureKey(key string, value interface{}) (string, map[string]string, map[string]interface{}) {
	// Beginning of Influx point
	meas := ""
	tags := make(map[string]string, 0)
	fields := make(map[string]interface{}, 0)

	// BracketSplit splits the metics on the "["
	bs := strings.Split(key, "[")
	// PeriodSplit splits the first part of the metric on "."s
	ps := strings.Split(bs[0], ".")

	// Switch on the results of the bracket split
	switch len(bs) {

	// No brackets so len(split) == 1
	case 1:

		// Switch on the results of the period split
		switch len(ps) {

		// meas.field
		case 2:
			meas = ps[0]
			fields[ps[1]] = value

		// meas.field*
		case 3:
			meas = ps[0]
			fields[fmt.Sprintf("%v.%v", ps[1], ps[2])] = value

		// meas.field.field.context
		case 4:
			if strings.Contains(ps[3], "-") {
				meas = ps[0]
				fields[fmt.Sprintf("%v.%v", ps[1], ps[2])] = value
				tags["context"] = ps[3]
			} else {
				meas = ps[0]
				fields[fmt.Sprintf("%v.%v.%v", ps[1], ps[2], ps[3])] = value
			}

		// Default
		default:
			meas = key
			fields["value"] = value
		}

	// Brackets so len(split) == 2
	// longest case
	case 2:

		// Switch on the results of the period split
		switch len(ps) {

		// period split only contains measurement
		case 1:
			meas = ps[0]
			bracket := trim(bs[1])
			// Arcane parsing rules
			switch {

			// Bracket contains something like 1/40 -> ignore
			case strings.Contains(bs[1], "/"):
				fields["value"] = value

			// bracket is field name wiht some changes
			case strings.Contains(bs[1], ","):
				// switch "," and " " to "."
				bracket = rp(rp(bracket, ",", "."), " ", ".")
				fields[bracket] = value

			// Default
			default:
				// log.Printf("HITTING DEFAULT: %v\n", key)
				meas = key
				fields["value"] = value
			}

		// period split contains more information as well as brackets
		case 2:
			meas = ps[0]
			bracket := trim(bs[1])
			// Switch on length of bracket
			switch {

			// short brakets
			case len(bracket) < 10:
				bracket = rp(bracket, ",", "")
				if bracket != "" {
					tags["process"] = bracket
				}
				fields[ps[1]] = value

			// medium brakets
			case len(bracket) < 25:
				// remove all {,}," from bracket
				bracket = rp(rp(rp(bracket, "\"", ""), "{", ""), "}", "")
				fields[bracket] = value

			// long brackets are system.run[curl ....]
			case len(bracket) > 25 && len(bracket) < 150:
				fields[ps[1]] = bracket
				tags["status_code"] = fmt.Sprint(value)

			// Default
			default:
				meas = key
				fields["value"] = value
			}

		// len(period_split) == 3 and contains more information
		case 3:
			meas = ps[0]
			bracket := trim(bs[1])

			// Switch on bracket content
			switch {

			// bracket contains context
			case strings.Contains(bracket, "-"):
				fields[jwp(ps[1], ps[2])] = value
				tags["context"] = bracket

			// bracket contains file system info
			case strings.Contains(bracket, "/"):
				t := strings.Split(bracket, ",")
				tags["path"] = t[0]
				fields[jw2p(ps[1], ps[2], t[1])] = value

			// TODO: find a non default case that fits all "net","system","vm" meass down here
			default:
				bracketCommaSplit := strings.Split(bracket, ",")

				// Switch on bracket contents then measurement (set on line 119)
				switch {

				// system cpu and swap meas
				case bracketCommaSplit[0] == "":
					fields[jwp(ps[1], bracketCommaSplit[1])] = value

				// net meas
				case meas == "net":
					tags["interface"] = bracketCommaSplit[0]
					if len(bracketCommaSplit) > 1 {
						fields[jw2p(ps[1], ps[2], bracketCommaSplit[1])] = value
					} else {
						fields[jwp(ps[1], ps[2])] = value
					}

				// vm measurement
				case meas == "vm":
					fields[jw2p(ps[1], ps[2], bracketCommaSplit[0])] = value

				// system measurment
				case meas == "system":
					// for per-cpu metrics we need to pull out cpu as tag
					if ps[1] == "cpu" {
						fields[jw2p(ps[1], ps[2], bracketCommaSplit[0])] = value
						tags["cpu"] = bracketCommaSplit[1]
					} else {
						// For system health checks we need to store system checked (mem, disk, cpu, etc...) with diff tags
						fields[jwp(ps[1], ps[2])] = value
						tags["system"] = bracketCommaSplit[0]
					}

				// web measurement
				case meas == "web":
					meas = jwp(ps[0], ps[1])
					if ps[2] == "time" {
						fields["value"] = value
					} else {
						fields[ps[2]] = value
					}
					tags["system"] = "ZabbixGUI"

				// Default
				default:
					meas = key
					fields["value"] = value
				}
			}

		// len(period_split) == 5 and contains most of the metadata
		case 5:
			meas = ps[0]
			bracket := trim(bs[1])
			// Switch on measurement name
			switch {

			// custom measurement -> custom.vfs.dev
			case meas == "custom":
				meas = jw2p(ps[0], ps[1], ps[2])
				tags["drive"] = bracket
				fields[jwp(ps[3], ps[4])] = value

			// app measurement
			case meas == "app":
				tags["name"] = jwp(ps[1], ps[2])
				fields[jwp(ps[3], ps[4])] = value

			// default
			default:
				meas = key
				fields["value"] = value
			}

		// Default case for len(period_split) == 5
		default:
			meas = key
			fields["value"] = value
		}

	// Multiple brackets -> grpavg["app-searchautocomplete","system.cpu.util[,user]",last,0]
	default:
		meas = key
		fields["value"] = value
	}
	// Return the start of a point
	return meas, tags, fields
}

// join with period
func jwp(s1, s2 string) string {
	return fmt.Sprintf("%v.%v", s1, s2)
}

// join with 2 period
func jw2p(s1, s2, s3 string) string {
	return fmt.Sprintf("%v.%v.%v", s1, s2, s3)
}

// replace
func rp(s, old, new string) string {
	return strings.Replace(s, old, new, -1)
}

// trims last char from string
func trim(s string) string {
	return s[0 : len(s)-1]
}

// common interface for different datatypes
type message interface {
	point() *client.Point
}

// takes an irMessage -> float field
type floatMessage struct {
	host   string
	clock  time.Time
	value  float64
	key    string
	server string
}

// satisfies the message interface
func (fm *floatMessage) point() *client.Point {
	meas, tags, fields := structureKey(fm.key, fm.value)
	tags["host"] = fm.host
	tags["server"] = fm.server
	pt, err := client.NewPoint(meas, tags, fields, fm.clock)
	if err != nil {
		panic(fmt.Errorf("%v: creating float point", err))
	}
	return pt
}

// takes an irMessage -> string field
type strMessage struct {
	host   string
	clock  time.Time
	value  string
	key    string
	server string
}

// satisfies the message interface
func (sm *strMessage) point() *client.Point {
	meas, tags, fields := structureKey(sm.key, sm.value)
	tags["host"] = sm.host
	tags["server"] = sm.server
	pt, err := client.NewPoint(meas, tags, fields, sm.clock)
	if err != nil {
		panic(fmt.Errorf("%v: creating string point", err))
	}
	return pt
}

// Stop satisfies the telegraf.ServiceInput interface
func (rmq *RabbitMQParser) Stop() {
	rmq.Lock()
	defer rmq.Unlock()
	rmq.conn.Close()
	rmq.ch.Close()
}

func init() {
	inputs.Add("rabbit_mq_parser", func() telegraf.Input {
		return &RabbitMQParser{}
	})
}