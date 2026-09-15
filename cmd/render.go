package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Palats/mapshot/embed"
	"github.com/Palats/mapshot/factorio"
	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/otiai10/copy"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// RenderFlags holds parameters to the rendering.
type RenderFlags struct {
	area          string
	tilemin       int64
	tilemax       int64
	prefix        string
	resolution    int64
	jpgquality    int64
	minjpgquality int64
	surface       string
}

// Register creates flags for the rendering parameters.
func (rf *RenderFlags) Register(flags *pflag.FlagSet, prefix string) *RenderFlags {
	flags.StringVar(&rf.area, prefix+"area", "", "How to pick the area to render. all=all existing chunks; player=chunks including entities of 'player' force; entities=chunks including whitelisted entities to match artificial builds. If empty, use value from the game.")
	flags.Int64Var(&rf.tilemin, prefix+"tilemin", 0, "Size in in-game units of a tile for the most zoomed layer. If 0, use value from the game.")
	flags.Int64Var(&rf.tilemax, prefix+"tilemax", 0, "Size in in-game units of a tile for the least zoomed layer. If 0, use value from the game.")
	flags.StringVar(&rf.prefix, prefix+"prefix", "", "Prefix to add to all generated filenames. If empty, use value from the game.")
	flags.Int64Var(&rf.resolution, prefix+"resolution", 0, "Pixel size for generated tiles. If 0, use value from the game.")
	flags.Int64Var(&rf.jpgquality, prefix+"jpgquality", 0, "Compression quality for jpg files. If 0, use value from the game.")
	flags.Int64Var(&rf.minjpgquality, prefix+"minjpgquality", -1, "Compression quality for jpg files when no player entities are present. Set to 0 to skip the tile entirely.")
	flags.StringVar(&rf.surface, prefix+"surface", "", "Game surface to render. If empty, use value from the game. Use _all_ for render all surfaces (default behavior).")
	return rf
}

func (rf *RenderFlags) genOverrides() map[string]interface{} {
	ov := map[string]interface{}{}
	if rf.area != "" {
		ov["area"] = rf.area
	}
	if rf.tilemin != 0 {
		ov["tilemin"] = rf.tilemin
	}
	if rf.tilemax != 0 {
		ov["tilemax"] = rf.tilemax
	}
	if rf.prefix != "" {
		ov["prefix"] = rf.prefix
	}
	if rf.resolution != 0 {
		ov["resolution"] = rf.resolution
	}
	if rf.jpgquality != 0 {
		ov["jpgquality"] = rf.jpgquality
	}
	if rf.jpgquality != -1 {
		ov["minjpgquality"] = rf.minjpgquality
	}
	if rf.surface != "" {
		ov["surface"] = rf.surface
	}
	return ov
}

func copyMod(dstMapshot string) error {
	if err := os.MkdirAll(dstMapshot, 0755); err != nil {
		return fmt.Errorf("unable to create dir %q: %w", dstMapshot, err)
	}
	for name, content := range embed.ModFiles {
		dst := filepath.Join(dstMapshot, name)
		if err := ioutil.WriteFile(dst, []byte(content), 0644); err != nil {
			return fmt.Errorf("unable to write file %q: %w", dst, err)
		}
	}
	return nil
}

// findTilePlan looks for the ntiles.txt plan file written by the mod after
// all screenshots have been queued. It returns the data prefix (relative to
// the script-output directory, slash separated with a trailing slash - the
// same format as the done file content) and the number of tile jpgs expected
// on disk once rendering is complete.
func findTilePlan(scriptOutput string) (string, int, error) {
	var planFile string
	err := filepath.Walk(scriptOutput, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "ntiles.txt" {
			planFile = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil || planFile == "" {
		return "", 0, fmt.Errorf("no tile plan found under %s", scriptOutput)
	}
	raw, err := ioutil.ReadFile(planFile)
	if err != nil {
		return "", 0, err
	}
	wanted, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || wanted < 0 {
		return "", 0, fmt.Errorf("invalid tile plan %q: %v", string(raw), err)
	}
	rel, err := filepath.Rel(scriptOutput, filepath.Dir(planFile))
	if err != nil {
		return "", 0, err
	}
	return filepath.ToSlash(rel) + "/", wanted, nil
}

// countTileJpgs counts the tile jpg files already written under dir.
func countTileJpgs(dir string) (int, error) {
	n := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.EqualFold(filepath.Ext(path), ".jpg") {
			n++
		}
		return nil
	})
	return n, err
}

func writeOverrides(data map[string]interface{}, dstPath string) error {
	inline, err := json.Marshal(data)
	if err != nil {
		return err
	}
	overrides := "return [===[" + string(inline) + "]===]\n"
	overridesFilename := filepath.Join(dstPath, "overrides.lua")
	if err := ioutil.WriteFile(overridesFilename, []byte(overrides), 0644); err != nil {
		return fmt.Errorf("unable to write overrides file %q: %w", overridesFilename, err)
	}
	glog.Infof("overrides file created at %q", overridesFilename)
	return nil
}

func render(ctx context.Context, factorioSettings *factorio.Settings, rf *RenderFlags, rawname string) error {
	fact, err := factorio.New(factorioSettings)
	if err != nil {
		return err
	}

	runID := uuid.New().String()
	glog.Infof("runid: %s", runID)

	// The parameter can be a filename, so extract a name.
	name := filepath.Base(rawname)
	name = name[:len(name)-len(filepath.Ext(name))]

	tmpdir, cleanup := getWorkDir()
	defer cleanup()

	// Copy game save
	srcSavegame, err := fact.FindSaveFile(rawname)
	if err != nil {
		return fmt.Errorf("unable to find savegame %q: %w", rawname, err)
	}
	fmt.Printf("Generating mapshot %q using file %s\n", name, srcSavegame)

	dstSavegame := filepath.Join(tmpdir, name+".zip")
	if err := copy.Copy(srcSavegame, dstSavegame); err != nil {
		return fmt.Errorf("unable to copy file %q: %w", srcSavegame, err)
	}
	glog.Infof("copied save from %q to %q", srcSavegame, dstSavegame)

	// Copy mods
	dstMods := filepath.Join(tmpdir, "mods")
	if err := fact.CopyMods(dstMods, []string{"mapshot"}); err != nil {
		return err
	}

	// Add the mod itself.
	dstMapshot := filepath.Join(dstMods, "mapshot")
	if err := copyMod(dstMapshot); err != nil {
		return err
	}
	if err := factorio.EnableMod(dstMods, "mapshot"); err != nil {
		return err
	}
	glog.Infof("mod created at %q", dstMapshot)

	// Generates overrides to the parameters. This is done by creating a Lua
	// file, as mods don't have any way of loading data.
	overridesData := rf.genOverrides()
	overridesData["onstartup"] = runID
	overridesData["savename"] = name
	if err := writeOverrides(overridesData, dstMapshot); err != nil {
		return err
	}

	// Remove done marker if still present
	doneFile := filepath.Join(fact.ScriptOutput(), "mapshot-done-"+runID)
	err = os.Remove(doneFile)
	glog.Infof("removed done-file %q: %v", doneFile, err)

	factorioArgs := []string{
		"--disable-audio",
		"--load-game", dstSavegame,
		"--mod-directory", dstMods,
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error)
	fmt.Println("Starting Factorio...")
	go func() {
		errCh <- fact.Run(execCtx, factorioArgs)
	}()

	// Wait for completion. Two signals, whichever comes first:
	//  - the legacy `done` file, written by the mod on Factorio <= 2.0 after
	//    set_wait_for_screenshots_to_finish() returned;
	//  - the tile plan file (ntiles.txt) plus counting the tile jpg files on
	//    disk, for Factorio 2.1 where the mod pauses the simulation instead
	//    of blocking on set_wait_for_screenshots_to_finish (which never
	//    returns there) and therefore never writes the done file.
	var resultPrefix string
	var tilesWanted int
	for {
		if _, err := os.Stat(doneFile); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("unable to stat file %q: %w", doneFile, err)
			}
		} else {
			cancel()
			break
		}

		if resultPrefix == "" {
			if prefix, wanted, err := findTilePlan(fact.ScriptOutput()); err == nil {
				resultPrefix = prefix
				tilesWanted = wanted
				glog.Infof("tile plan found: %d tile(s) expected under %q", tilesWanted, resultPrefix)
			}
		}
		if resultPrefix != "" {
			n, err := countTileJpgs(filepath.Join(fact.ScriptOutput(), resultPrefix))
			if err == nil && n >= tilesWanted {
				glog.Infof("all %d tile(s) written (file-count completion)", n)
				cancel()
				break
			}
		}

		// Context cancellation should terminate Factorio, which is detected
		// through errCh, so no need to wait on context.
		select {
		case <-time.After(time.Second):
		case err := <-errCh:
			if err == nil {
				return errors.New("factorio exited early")
			}
			return fmt.Errorf("factorio exited early: %w", err)
		}
	}

	if resultPrefix == "" {
		// Legacy path: read the output prefix from the done file.
		glog.Infof("done file %q now exists", doneFile)
		rawDone, err := ioutil.ReadFile(doneFile)
		if err != nil {
			return fmt.Errorf("unable to read file %q: %w", doneFile, err)
		}
		resultPrefix = string(rawDone)

		// Cleaning up done file now that we've read it.
		err = os.Remove(doneFile)
		glog.Infof("removed done-file %q: %v", doneFile, err)
	}
	glog.Infof("output at %s", resultPrefix)
	fmt.Println("Output:", filepath.Join(fact.ScriptOutput(), resultPrefix))

	// Wait for Factorio to terminate.
	err = <-errCh
	if err != nil {
		glog.Warningf("Factorio finished with an error; ignoring as rendering was done. Error: %v", err)
	}

	return nil
}

var cmdRender = &cobra.Command{
	Use:   "render",
	Short: "Create a screenshot from a save.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return render(cmd.Context(), factorioSettings, renderFlags, args[0])
	},
}

var renderFlags = &RenderFlags{}

func init() {
	renderFlags.Register(cmdRender.PersistentFlags(), "")
	cmdRoot.AddCommand(cmdRender)
}
