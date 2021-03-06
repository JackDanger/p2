package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/square/p2/pkg/kp"
	"github.com/square/p2/pkg/kp/flags"
	"github.com/square/p2/pkg/labels"
	"gopkg.in/alecthomas/kingpin.v2"
	klabels "k8s.io/kubernetes/pkg/labels"
)

var (
	cmdApply              = kingpin.Command(CmdApply, "Apply label changes to all objects matching a selector")
	applyAutoConfirm      = cmdApply.Flag("yes", "Autoconfirm label applications. Use with caution!").Short('y').Bool()
	applyLabelType        = cmdApply.Flag("labelType", "The type of label to adjust. Sometimes called the \"label tree\". Supported types can be found here:\n\thttps://godoc.org/github.com/square/p2/pkg/labels#pkg-constants").Short('t').Required().String()
	applySubjectSelector  = cmdApply.Flag("selector", "The selector on which to modify labels.").Short('s').Required().String()
	applyAddititiveLabels = cmdApply.Flag("add", `The label set to apply to the subject. Include multiple --add switches to include multiple labels. It's safe to mix --add with --delete though the results of this command are not transactional.

Example:
    p2-label --selector $selector --add foo=bar --add bar=baz
`).Short('a').StringMap()
	applyDestructiveLabels = cmdApply.Flag("delete", `The list of label keys to remove from the nodes in the selector. Deletes are idempotent. Include multiple --delete switches to include multiple labels. It's safe to mix --add with --delete though the results of this command are not transactional.

Example:
  p2-label --selector $selector --delete foo --delete bar
`).Short('d').Strings()

	cmdShow       = kingpin.Command(CmdShow, "Show labels that apply to a particular entity (type, ID)")
	showLabelType = cmdShow.Flag("labelType", "The type of label to adjust. Sometimes called the \"label tree\". Supported types can be found here:\n\thttps://godoc.org/github.com/square/p2/pkg/labels#pkg-constants").Short('t').Required().String()
	showID        = cmdShow.Flag("id", "The ID of the entity to show labels for.").Short('i').Required().String()

	// autoConfirm captures the confirmation desire abstractly across commands
	autoConfirm = false
)

const (
	CmdApply = "apply"
	CmdShow  = "show"
)

func main() {
	cmd, opts := flags.ParseWithConsulOptions()
	client := kp.NewConsulClient(opts)
	applicator := labels.NewConsulApplicator(client, 3)
	exitCode := 0

	switch cmd {
	case CmdShow:
		labelType, err := labels.AsType(*showLabelType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error while parsing label type. Check the commandline.\n%v\n", err)
			exitCode = 1
			break
		}

		labelsForEntity, err := applicator.GetLabels(labelType, *showID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Got error while querying labels. %v\n", err)
			exitCode = 1
			break
		}
		fmt.Printf("%s/%s: %s\n", labelType, *showID, labelsForEntity.Labels.String())
		return
	case CmdApply:
		autoConfirm = *applyAutoConfirm

		labelType, err := labels.AsType(*applyLabelType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unrecognized type %s. Check the commandline and documentation.\nhttps://godoc.org/github.com/square/p2/pkg/labels#pkg-constants\n", *applyLabelType)
			exitCode = 1
			break
		}

		subject, err := klabels.Parse(*applySubjectSelector)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error while parsing subject label. Check the syntax.\n%v\n", err)
			break
		}

		additiveLabels := *applyAddititiveLabels
		destructiveKeys := *applyDestructiveLabels

		cachedMatch := false
		matches, err := applicator.GetMatches(subject, labelType, cachedMatch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error while finding label matches. Check the syntax.\n%v\n", err)
			exitCode = 1
			break
		}

		if len(additiveLabels) > 0 {
			fmt.Printf("labels to be added: %s\n", klabels.Set(additiveLabels))
		}

		if len(destructiveKeys) > 0 {
			fmt.Printf("labels to be removed: %s\n", destructiveKeys)
		}

		var labelsForEntity labels.Labeled
		for _, match := range matches {
			entityID := match.ID

			err := applyLabels(applicator, entityID, labelType, additiveLabels, destructiveKeys)
			if err != nil {
				fmt.Printf("Encountered err during labeling, %v", err)
				exitCode = 1
			}

			labelsForEntity, err = applicator.GetLabels(labelType, entityID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Got error while querying labels. %v\n", err)
				exitCode = 1
				continue
			}
			fmt.Printf("%s/%s: %s\n", labelType, entityID, labelsForEntity.Labels.String())
		}
		break
	}

	os.Exit(exitCode)
}

func applyLabels(applicator labels.Applicator, entityID string, labelType labels.Type, additiveLabels map[string]string, destructiveKeys []string) error {
	var err error
	if !confirm(fmt.Sprintf("mutate the labels for %s/%s", labelType, entityID)) {
		return nil
	}
	if len(additiveLabels) > 0 {
		for k, v := range additiveLabels {
			err = applicator.SetLabel(labelType, entityID, k, v)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error while appyling label. k/v: %s/%s.\n%v\n", k, v, err)
			}
		}
	}
	if len(destructiveKeys) > 0 {
		for _, key := range destructiveKeys {
			err = applicator.RemoveLabel(labelType, entityID, key)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error while destroying label with key: %s.\n%v\n", key, err)
			}
		}
	}
	return nil
}

func confirm(message string) bool {
	if autoConfirm {
		return true
	}

	fmt.Printf("Confirm your intention to %s\n", message)
	fmt.Printf(`Type "y" to confirm [n]: `)
	var input string
	_, err := fmt.Scanln(&input)
	if err != nil {
		return false
	}
	resp := strings.TrimSpace(strings.ToLower(input))
	return resp == "y" || resp == "yes"
}
