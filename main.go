package main

/* p4unity
 * `change-content` handler for Perforce Helix to guard against
 * bad behaviour with Unity projects' .meta files
 *
 * harry denholm, 2020; ishani.org
 */

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chilts/sid"
	"go.uber.org/zap"
)

// ----------------------------------------------------------------------------------------------------------

// VerboseLogger produces a zap logger that writes to a new, unique log file for
// every invocation of p4unity, allowing for very verbose tracking of what's happening. Not intended
// for day to day use, as there's no log expiration or rotation - it will just sit there slowly filling up
// next to your P4 server instance
func VerboseLogger() (*zap.Logger, error) {

	os.Mkdir("p4unity_logs", os.ModePerm)

	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{
		fmt.Sprintf("p4unity_logs/%s.txt", sid.IdHex()), // not when invoked by p4, logs appear next to p4d/p4s.exe
	}
	return cfg.Build()
}

var zLog *zap.Logger = nil

// ----------------------------------------------------------------------------------------------------------
// custom app exit codes; anything other than 0 will halt the p4 process
// switching Success to return non-0 can help when testing against a live depot, so you can see the results
// of the logic without actually allowing anything to complete
//
const p4ExitSuccess = 0        // the commit is considered ok
const p4ExitBypass = 0         // a magic bypass code was in the commit text
const p4ExitProblems = 1       // there were problems, the commit should be re-examined
const p4ExitErrorException = 1 // there was an unexpected error
const p4ExitErrorEmpty = 1     // p4 returned empty results when interrogating about files or CLs
const p4ExitErrorUsage = 1     // missing arguments

// ----------------------------------------------------------------------------------------------------------
// simple type wrapper for a string set
//
type stringSet map[string]struct{}

func (s stringSet) add(strvalue string) {
	s[strvalue] = struct{}{}
}

func (s stringSet) remove(strvalue string) {
	delete(s, strvalue)
}

func (s stringSet) has(strvalue string) bool {
	_, ok := s[strvalue]
	return ok
}

// ----------------------------------------------------------------------------------------------------------
// p4 operations by context
//
var opsAdd = stringSet{
	"move/add": {},
	"add":      {},
	"import":   {},
}
var opsDel = stringSet{
	"move/delete": {},
	"delete":      {},
	"purge":       {},
	"archive":     {},
}
var opsExists = stringSet{
	"edit":     {},
	"move/add": {},
	"add":      {},
	"import":   {},
}

// ----------------------------------------------------------------------------------------------------------
// precompiled regular expressions used below
//

// snap a record into the path, #revision and operation (eg. edit, add..)
var reFileRecordUnpack = regexp.MustCompile(`(?m)([^#]+)#(\d+) ([\w\/]+)$`)

// extract just the "headAction <operation>" state line from a fstat call
var reFindHeadActionOp = regexp.MustCompile(`(?m)headAction\s+([\w\/]+)`)

// <file> - no file(s) at that changelist number. <- files exist, but not at given CL
// <file> - no such file(s).                      <- files not known to P4 at all
var reNoFilesMatch = regexp.MustCompile(`no\s+(?:such)?\s?file\(s\)`)

// ----------------------------------------------------------------------------------------------------------
// given the result of a p4 command executed with -s, return just the lines with the prefix <p4type>; eg. "info1"
// (with the prefix removed)
//
func filterStringsByType(s []string, p4type string) []string {
	result := make([]string, 0, len(s))
	cutTypeLen := len(p4type)
	for _, str := range s {
		if strings.HasPrefix(str, p4type) {
			str = strings.TrimSpace(str[cutTypeLen:])
			if str != "" {
				result = append(result, str)
			}
		}
	}
	return result
}

// ----------------------------------------------------------------------------------------------------------
//
func fileExistsInDepot(depotPath string) (bool, error) {

	cmd := exec.Command(
		"p4",
		"-p", AppConfig.PerforceServer,
		"-u", AppConfig.PerforceUser,
		"-P", AppConfig.PerforcePass,
		"-s",
		"fstat",
		depotPath,
	)
	fstatOut, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("[p4unity] failed to launch P4; %s\n%s\n\n", err, fstatOut)
		return false, err
	}

	fstatOutString := string(fstatOut)
	zLog.Info("fstat", zap.String("out", fstatOutString))

	fstatHeadAction := reFindHeadActionOp.FindStringSubmatch(fstatOutString)

	if len(fstatHeadAction) == 0 {
		zLog.Info("fstat", zap.String("failed", "regex fail"))
		return false, nil
	}

	// check if the head action is appropriate; eg. add, edit - something that infers this file in the depot at this time
	fstatHeadActionOp := fstatHeadAction[1]

	if !opsExists.has(fstatHeadActionOp) {
		zLog.Info("fstat", zap.String("ignored_action", fstatHeadActionOp))
		return false, nil
	}

	return true, nil
}

// ----------------------------------------------------------------------------------------------------------
func app() int {

	argsWithoutProg := os.Args[1:]
	fmt.Print("\n\n")
	zLog.Info("Boot", zap.Strings("args", argsWithoutProg))

	if len(argsWithoutProg) < 1 {
		fmt.Printf("usage: p4unity <changelist>\n\n")
		return p4ExitErrorUsage
	}

	// check we got a changelist number on the command line
	changelist, err := strconv.Atoi(argsWithoutProg[0])
	if err != nil {
		fmt.Printf("[p4unity] changelist %s not a number (%s)\n\n", argsWithoutProg[0], err)
		return p4ExitErrorUsage
	}

	// talk to p4, get the description of the given changelist
	cmd := exec.Command(
		"p4",
		"-p", AppConfig.PerforceServer,
		"-u", AppConfig.PerforceUser,
		"-P", AppConfig.PerforcePass,
		"-s",
		"describe",
		"-s",
		strconv.FormatInt(int64(changelist), 10),
	)
	p4out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("[p4unity] failed to launch P4; %s\n%s\n\n", err, p4out)
		return p4ExitErrorUsage
	}

	// log out the result for tracing
	p4outString := string(p4out)
	zLog.Info("p4-describe", zap.String("output", p4outString))

	// turn the result into individual lines we can step through
	p4lines := strings.Split(p4outString, "\r\n")
	zLog.Info("p4-describe", zap.Int("split-lines", len(p4lines)))

	// early out if we asked for a missing CL; this would mean p4d screwed up somehow? how can we fire a trigger for a CL that doesn't exist...
	if strings.Contains(p4lines[0], "no such changelist") {
		fmt.Printf("[p4unity] cannot find changelist [%d]\n\n", changelist)
		return p4ExitErrorUsage
	}

	// strip into the header text and info blocks; running the p4 '-s' global flag
	// usefully separates the output; https://community.perforce.com/s/article/3505
	//
	// [text: Change 9148 by harry_denholm@harry_pc on 2020/01/01 11:11:11 *pending*]
	// [text: ]
	// [text:  Example changelist]
	// [text: ]
	// [text: Affected files ...]
	// [text: ]
	// [info1: //Depot/UnityProjects/Thing/Assets/Native/Binding.cs.meta#1 add]
	// [info1: //Depot/UnityProjects/Thing/Assets/Native/Binding.meta#1 add]
	// ...

	p4text := filterStringsByType(p4lines, "text:")
	p4info := filterStringsByType(p4lines, "info1:")

	p4headerLines := len(p4text)
	p4fileCount := len(p4info)

	zLog.Info("filtering",
		zap.Int("p4headerLines", p4headerLines),
		zap.Int("p4fileCount", p4fileCount),
	)

	// no header, no idea
	if p4headerLines == 0 {
		fmt.Printf("[p4unity] p4 describe [%d] output is empty\n\n", changelist)
		return p4ExitErrorEmpty
	}

	// no files, no point
	if p4fileCount == 0 {
		fmt.Printf("[p4unity] changelist [%d] has no file records?\n\n", changelist)
		return p4ExitErrorEmpty
	}

	// look through the commit message; if we have any magic words to bypass this check, abort early
	for i := 1; i < p4headerLines; i++ {
		if strings.Contains(p4text[i], AppConfig.BypassKeyphrase) {
			fmt.Printf("[p4unity] bypassing validation\n\n")
			zLog.Info("bypassed")
			return p4ExitBypass
		}
	}

	filesBeingAdded := make(stringSet)
	filesBeingAddedIgnoringCase := make(stringSet)
	filesBeingDeleted := make(stringSet)
	filesBeingDeletedIgnoringCase := make(stringSet)

	for pi := 0; pi < p4fileCount; pi++ {

		item := p4info[pi]

		// carve up the line, eg
		// "//Depot/UnityProjects/Thing/Assets/Native/Binding.cs.meta#1 add"
		matches := reFileRecordUnpack.FindStringSubmatch(item)

		// we expect 4 groups; [all], [file], [revision], [operation]
		// it would be a serious error if our regex can't process something, so flag it up
		if len(matches) != 4 {
			fmt.Printf("[p4unity] file parse failed for '%s'\n\n", item)
			return p4ExitErrorException
		}

		filePath := matches[1]
		vcsOperation := matches[3]
		itemDirectory, itemFilename := filepath.Split(filePath)

		// create logging structure for this item
		itemLog := zLog.With(zap.String("original-spec", item))

		// log the entry as all the bits we've cut it into
		itemLog.Info("Candidate",
			zap.Strings("elements", matches),
			zap.Int("index", pi),
			zap.String("dir-part", itemDirectory),
			zap.String("file-part", itemFilename),
		)

		// a directory that terminates with a ~ should be ignored; everything within will not be treated as imported assets
		if strings.Contains(itemDirectory, "~/") {
			itemLog.Info("TildeIgnored")
			continue
		}

		// ignore .p4ignore, .tests.json et al
		if strings.HasPrefix(itemFilename, ".") {
			itemLog.Info("DotIgnored")
			continue
		}

		// check the whitelist to see if we should be looking at this file at all
		pathIsValidToCheck := false
		for _, whitelist := range AppConfig.PathWhitelist {
			if strings.HasPrefix(itemDirectory, whitelist) {
				itemLog.Info("Whitelist", zap.String("passed", whitelist))
				pathIsValidToCheck = true
				break
			}
		}
		if !pathIsValidToCheck {
			itemLog.Info("Whitelist-Failed")
			continue
		}

		// this is a shitty vague way of only apply rules to the inside of Unity assets folders
		// TBD: maybe either explicitly use a path list .. or something else, like fstat'ing a sibling path of "/Packages/" for example
		if strings.Contains(itemDirectory, "/Assets/") == false {
			itemLog.Info("AssetsPath-Failed")
			continue
		}

		// group files by operation
		if opsAdd.has(vcsOperation) {
			itemLog.Info("MarkedForAdd")
			filesBeingAdded.add(filePath)
			filesBeingAddedIgnoringCase.add(strings.ToLower(filePath))
		}
		if opsDel.has(vcsOperation) {
			itemLog.Info("MarkedForDelete")
			filesBeingDeleted.add(filePath)
			filesBeingDeletedIgnoringCase.add(strings.ToLower(filePath))
		}
	}

	allowCommitToContinue := true

	// --------------------------------------------------------
	zLog.Info("Checking ADD list", zap.Int("count", len(filesBeingAdded)))
	for fadd := range filesBeingAdded {

		fileExtension := filepath.Ext(fadd)

		// file is an asset; check to see if there's a .meta accompaniment
		if fileExtension != ".meta" {

			fileWithMeta := fadd + ".meta"

			// is the meta file coming in this changelist? that's nice
			if filesBeingAdded.has(fileWithMeta) {
				continue
			}
			// in ignore-case mode, also check the lowered list
			if filesBeingAddedIgnoringCase.has(strings.ToLower(fileWithMeta)) {
				continue
			}

			// if it's not in the changelist, is it already in the depot at time of commit?
			foundInDepot, err := fileExistsInDepot(fileWithMeta)
			if err != nil {
				fmt.Printf("[p4unity] fstat failed for '%s'\n( %s )\n", fileWithMeta, err)
				return p4ExitErrorException
			}

			if foundInDepot {
				continue
			}

			fmt.Printf("Missing .meta file for '%s'\n", fadd)
			allowCommitToContinue = false

		} else {
			// .. otherwise, it's a meta file; see if we can determine if it represents a directory or an asset

			fileWithoutMeta := fadd[0 : len(fadd)-len(".meta")]

			// removing extension again can indicate if this is a meta for a directory (or, technically, an extensionless asset, but whatchagondo)
			remainingExtension := strings.TrimSpace(filepath.Ext(fileWithoutMeta))
			if len(remainingExtension) == 0 {
				// there's no matching P4 entry for a directory, so we have to just assume and let this pass
				continue
			}

			// the asset is in the changelist, well alright then
			if filesBeingAdded.has(fileWithoutMeta) {
				continue
			}
			// in ignore-case mode, also check the lowered list
			if filesBeingAddedIgnoringCase.has(strings.ToLower(fileWithoutMeta)) {
				continue
			}

			// if it's not in the changelist, is it already in the depot at time of commit?
			foundInDepot, err := fileExistsInDepot(fileWithoutMeta)
			if err != nil {
				fmt.Printf("[p4unity] fstat failed for '%s'\n( %s )\n", fileWithoutMeta, err)
				return p4ExitErrorException
			}

			if foundInDepot {
				continue
			}

			fmt.Printf("Missing asset for .meta file '%s'\n", fadd)
			allowCommitToContinue = false
		}
	}

	// --------------------------------------------------------
	zLog.Info("Checking DEL list", zap.Int("count", len(filesBeingDeleted)))
	for fdel := range filesBeingDeleted {

		fileExtension := filepath.Ext(fdel)

		if fileExtension != ".meta" {

			fileWithMeta := fdel + ".meta"

			// file's twin is being deleted as part of this CL, all is well
			if filesBeingDeleted.has(fileWithMeta) {
				continue
			}
			// in ignore-case mode, also check the lowered list
			if filesBeingDeletedIgnoringCase.has(strings.ToLower(fileWithMeta)) {
				continue
			}

			// if the meta isn't being deleted now, maybe it's already deleted (and we're tidying up)
			foundInDepot, err := fileExistsInDepot(fileWithMeta)
			if err != nil {
				fmt.Printf("[p4unity] fstat failed for '%s'\n( %s )\n", fdel, err)
				return p4ExitErrorException
			}

			if !foundInDepot {
				continue
			}

			fmt.Printf("Need to delete the orphaned .meta for '%s'\n", fdel)
			allowCommitToContinue = false

		} else {

			// fileWithoutMeta := fdel[0 : len(fdel)-len(".meta")]
			// TBD
		}

	}

	if allowCommitToContinue {
		fmt.Println("success")
		return p4ExitSuccess
	}

	return p4ExitProblems
}

// ----------------------------------------------------------------------------------------------------------
func main() {

	perfStart := time.Now()

	LoadConfig()

	if AppConfig.VerboseLogs {

		// spin up a log
		var err error
		zLog, err = VerboseLogger()
		if err != nil {
			log.Panicf("[p4unity] could not open log\n( %s )\n", err)
		}

	} else {

		// log to the void
		zLog = zap.NewNop()

	}

	exitCode := app()

	perfElapsed := fmt.Sprintf("%s", time.Since(perfStart))
	zLog.Info("Performance", zap.String("elapsed", perfElapsed))

	os.Exit(exitCode)
}
