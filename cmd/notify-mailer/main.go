package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/mail"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jmhodges/clock"
	"gopkg.in/gorp.v1"

	"github.com/letsencrypt/boulder/cmd"
	blog "github.com/letsencrypt/boulder/log"
	bmail "github.com/letsencrypt/boulder/mail"
	"github.com/letsencrypt/boulder/sa"
)

type mailer struct {
	clk           clock.Clock
	log           blog.Logger
	dbMap         *gorp.DbMap
	mailer        bmail.Mailer
	subject       string
	emailTemplate string
	destinations  []string
	checkpoint    interval
	sleepInterval time.Duration
}

type interval struct {
	start int
	end   int
}

func (i *interval) ok() error {
	if i.start < 0 || i.end < 0 {
		return fmt.Errorf(
			"interval start (%d) and end (%d) must both be positive integers",
			i.start, i.end)
	}

	if i.start > i.end && i.end != 0 {
		return fmt.Errorf(
			"interval start value (%d) is greater than end value (%d)",
			i.start, i.end)
	}

	return nil
}

func (m *mailer) ok() error {
	// Make sure the checkpoint range is OK
	if checkpointErr := m.checkpoint.ok(); checkpointErr != nil {
		return checkpointErr
	}

	// Do not allow a start larger than the # of destinations
	if m.checkpoint.start > len(m.destinations) {
		return fmt.Errorf(
			"interval start value (%d) is greater than number of destinations (%d)",
			m.checkpoint.start,
			len(m.destinations))
	}

	// Do not allow a negative sleep interval
	if m.sleepInterval < 0 {
		return fmt.Errorf(
			"sleep interval (%d) is < 0", m.sleepInterval)
	}

	return nil
}

func (m *mailer) run() error {
	if err := m.ok(); err != nil {
		return err
	}
	// If there is no endpoint specified, use the total # of destinations
	if m.checkpoint.end == 0 {
		m.checkpoint.end = len(m.destinations)
	}
	for _, dest := range m.destinations[m.checkpoint.start:m.checkpoint.end] {
		if strings.TrimSpace(dest) == "" {
			continue
		}
		err := m.mailer.SendMail([]string{dest}, m.subject, m.emailTemplate)
		if err != nil {
			return err
		}
		m.clk.Sleep(m.sleepInterval)
	}
	return nil
}

// Since the only thing we use from gorp is the SelectOne method on the
// gorp.DbMap object, we just define an interface with that method
// instead of importing all of gorp. This facilitates mock implementations for
// unit tests
type dbSelector interface {
	SelectOne(holder interface{}, query string, args ...interface{}) error
}

// Updates a bmail.MailerDestination using the reg ID to ensure the "freshest"
// contact field.
func updateEmail(contact *bmail.MailerDestination, dbMap dbSelector) error {
	id := contact.ID
	// Select fields into the contact object
	err := dbMap.SelectOne(contact,
		`SELECT id, contact
		FROM registrations
		WHERE contact != 'null' AND id = :id;`,
		map[string]interface{}{
			"id": id,
		})
	if err != nil {
		return err
	}
	// Update the Email field using the contact JSON
	return contact.UnmarshalEmail()
}

// Update each `MailerDestination` to the most up-to-date contact email, convert
// to a slice of email addresses and return both deduplicated and sorted.
func resolveDestinations(contacts []*bmail.MailerDestination, dbMap dbSelector) ([]string, error) {
	contactMap := make(map[string]struct{}, len(contacts))
	for _, c := range contacts {
		err := updateEmail(c, dbMap)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(c.Email) == "" {
			continue
		}
		// Using the contactMap to deduplicate addresses
		contactMap[c.Email] = struct{}{}
	}

	var contactsList []string
	// Convert the de-dupe'd map back to a slice, sort it
	for contact := range contactMap {
		contactsList = append(contactsList, contact)
	}
	sort.Strings(contactsList)
	return contactsList, nil
}

func main() {
	from := flag.String("from", "", "From header for emails. Must be a bare email address.")
	subject := flag.String("subject", "", "Subject of emails")
	toFile := flag.String("toFile", "", "File containing a list of email addresses to send to, one per file.")
	toFileEmails := flag.Bool("emails", false, "toFile contains email addresses (default: reg. IDs)")
	bodyFile := flag.String("body", "", "File containing the email body in plain text format.")
	dryRun := flag.Bool("dryRun", true, "Whether to do a dry run.")
	sleep := flag.Duration("sleep", 60*time.Second, "How long to sleep between emails.")
	start := flag.Int("start", 0, "Line of input file to start from.")
	end := flag.Int("end", 99999999, "Line of input file to end before.")
	type config struct {
		NotifyMailer struct {
			cmd.DBConfig
			cmd.PasswordConfig
			cmd.SMTPConfig
		}
	}
	configFile := flag.String("config", "", "File containing a JSON config.")

	flag.Parse()
	if from == nil || subject == nil || bodyFile == nil || configFile == nil {
		flag.Usage()
		os.Exit(1)
	}

	_, log := cmd.StatsAndLogging(cmd.StatsdConfig{}, cmd.SyslogConfig{StdoutLevel: 7})

	configData, err := ioutil.ReadFile(*configFile)
	cmd.FailOnError(err, fmt.Sprintf("Reading %s", *configFile))
	var cfg config
	err = json.Unmarshal(configData, &cfg)
	cmd.FailOnError(err, "Unmarshaling config")

	dbURL, err := cfg.NotifyMailer.DBConfig.URL()
	cmd.FailOnError(err, "Couldn't load DB URL")
	dbMap, err := sa.NewDbMap(dbURL, 10)
	cmd.FailOnError(err, "Could not connect to database")

	// Load email body
	body, err := ioutil.ReadFile(*bodyFile)
	cmd.FailOnError(err, fmt.Sprintf("Reading %s", *bodyFile))

	address, err := mail.ParseAddress(*from)
	cmd.FailOnError(err, fmt.Sprintf("Parsing %s", *from))

	toBody, err := ioutil.ReadFile(*toFile)
	cmd.FailOnError(err, fmt.Sprintf("Reading %s", *toFile))

	var destinations []string
	if *toFileEmails {
		// If the toFile is full of bare email addresses, use them as-is for the
		// destinations, no processing required
		destinations = strings.Split(string(toBody), "\n")
	} else {
		// Otherwise, we have a file of JSON MailerDestinations to unmarshal
		var contacts []*bmail.MailerDestination
		err := json.Unmarshal(toBody, &contacts)
		cmd.FailOnError(err, fmt.Sprintf("Unmarshaling %s", *toFile))
		// Resolving the MailerDestinations to de-dupe'd email addresses and use
		// that for the mail destinations
		destinations, err = resolveDestinations(contacts, dbMap)
		cmd.FailOnError(err, "Resolving emails")
	}

	checkpointRange := interval{
		start: *start,
		end:   *end,
	}

	var mailClient bmail.Mailer
	if *dryRun {
		mailClient = bmail.NewDryRun(*address, log)
	} else {
		smtpPassword, err := cfg.NotifyMailer.PasswordConfig.Pass()
		cmd.FailOnError(err, "Failed to load SMTP password")
		mailClient = bmail.New(
			cfg.NotifyMailer.Server,
			cfg.NotifyMailer.Port,
			cfg.NotifyMailer.Username,
			smtpPassword,
			*address)
	}
	err = mailClient.Connect()
	cmd.FailOnError(err, fmt.Sprintf("Connecting to %s:%s",
		cfg.NotifyMailer.Server, cfg.NotifyMailer.Port))
	defer func() {
		err = mailClient.Close()
		cmd.FailOnError(err, "Closing mail client")
	}()

	m := mailer{
		clk:           cmd.Clock(),
		log:           log,
		dbMap:         dbMap,
		mailer:        mailClient,
		subject:       *subject,
		destinations:  destinations,
		emailTemplate: string(body),
		checkpoint:    checkpointRange,
		sleepInterval: *sleep,
	}

	err = m.run()
	cmd.FailOnError(err, "mailer.send returned error")
}