package rewards

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	rprewards "github.com/rocket-pool/rocketpool-go/rewards"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/rocket-pool/smartnode/shared/services/beacon"
	"github.com/rocket-pool/smartnode/shared/services/config"
	"github.com/rocket-pool/smartnode/shared/services/state"
	"github.com/rocket-pool/smartnode/shared/utils/log"
)

const (
	recordsFilenameFormat  string = "%d-%d.json.zst"
	recordsFilenamePattern string = "(?P<slot>\\d+)\\-(?P<epoch>\\d+)\\.json\\.zst"
	checksumTableFilename  string = "checksums.sha384"
)

// Manager for RollingRecords
type RollingRecordManager struct {
	Record                       *RollingRecord
	LatestFinalizedEpoch         uint64
	ExpectedBalancesBlock        uint64
	ExpectedRewardsIntervalBlock uint64

	log            *log.ColorLogger
	errLog         *log.ColorLogger
	logPrefix      string
	cfg            *config.RocketPoolConfig
	rp             *rocketpool.RocketPool
	bc             beacon.Client
	mgr            *state.NetworkStateManager
	startSlot      uint64
	lastSavedEpoch uint64

	beaconCfg            beacon.Eth2Config
	genesisTime          time.Time
	compressor           *zstd.Encoder
	decompressor         *zstd.Decoder
	recordsFilenameRegex *regexp.Regexp
}

// Creates a new manager for rolling records.
func NewRollingRecordManager(log *log.ColorLogger, errLog *log.ColorLogger, cfg *config.RocketPoolConfig, rp *rocketpool.RocketPool, bc beacon.Client, mgr *state.NetworkStateManager, startSlot uint64, beaconCfg beacon.Eth2Config, rewardsInterval uint64) (*RollingRecordManager, error) {
	// Get the Beacon genesis time
	genesisTime := time.Unix(int64(beaconCfg.GenesisTime), 0)

	// Create the zstd compressor and decompressor
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return nil, fmt.Errorf("error creating zstd compressor for rolling record manager: %w", err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("error creating zstd decompressor for rolling record manager: %w", err)
	}

	// Create the records filename regex
	recordsFilenameRegex := regexp.MustCompile(recordsFilenamePattern)

	// Make the records folder if it doesn't exist
	recordsPath := cfg.Smartnode.GetRecordsPath()
	fileInfo, err := os.Stat(recordsPath)
	if os.IsNotExist(err) {
		err2 := os.MkdirAll(recordsPath, 0755)
		if err2 != nil {
			return nil, fmt.Errorf("error creating rolling records folder: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("error checking rolling records folder: %w", err)
	} else if !fileInfo.IsDir() {
		return nil, fmt.Errorf("rolling records folder location exists (%s), but is not a folder", recordsPath)
	}

	logPrefix := "[Rolling Record]"
	log.Printlnf("%s Created Rolling Record manager for start slot %d.", logPrefix, startSlot)
	return &RollingRecordManager{
		Record: NewRollingRecord(log, logPrefix, bc, startSlot, &beaconCfg, rewardsInterval),

		log:                  log,
		errLog:               errLog,
		logPrefix:            logPrefix,
		cfg:                  cfg,
		rp:                   rp,
		bc:                   bc,
		mgr:                  mgr,
		startSlot:            startSlot,
		beaconCfg:            beaconCfg,
		genesisTime:          genesisTime,
		compressor:           encoder,
		decompressor:         decoder,
		recordsFilenameRegex: recordsFilenameRegex,
	}, nil
}

// Generate a new record for the provided slot using the latest viable saved record
func (r *RollingRecordManager) GenerateRecordForState(state *state.NetworkState) (*RollingRecord, error) {
	// Load the latest viable record
	slot := state.BeaconSlotNumber
	rewardsInterval := state.NetworkDetails.RewardIndex
	record, err := r.LoadBestRecordFromDisk(r.startSlot, slot, rewardsInterval)
	if err != nil {
		return nil, fmt.Errorf("error loading best record for slot %d: %w", slot, err)
	}

	if record.LastDutiesSlot == slot {
		// Already have a full snapshot so we don't have to do anything
		r.log.Printf("%s Loaded record was already up-to-date for slot %d.", r.logPrefix, slot)
		return record, nil
	} else if record.LastDutiesSlot > slot {
		// This should never happen but sanity check it anyway
		return nil, fmt.Errorf("loaded record has duties completed for slot %d, which is too far forward (targeting slot %d)", record.LastDutiesSlot, slot)
	}

	// Update to the target slot
	err = r.UpdateRecordToState(state, slot)
	if err != nil {
		return nil, fmt.Errorf("error updating record to slot %d: %w", slot, err)
	}

	return record, nil
}

// Save the rolling record to a file and update the record info catalog
func (r *RollingRecordManager) SaveRecordToFile(record *RollingRecord) error {

	// Serialize the record
	bytes, err := record.Serialize()
	if err != nil {
		return fmt.Errorf("error saving rolling record: %w", err)
	}

	// Compress the record
	compressedBytes := r.compressor.EncodeAll(bytes, make([]byte, 0, len(bytes)))

	// Get the record filename
	slot := record.LastDutiesSlot
	epoch := record.LastDutiesSlot / r.beaconCfg.SlotsPerEpoch
	recordsPath := r.cfg.Smartnode.GetRecordsPath()
	filename := filepath.Join(recordsPath, fmt.Sprintf(recordsFilenameFormat, slot, epoch))

	// Write it to a file
	err = os.WriteFile(filename, compressedBytes, 0664)
	if err != nil {
		return fmt.Errorf("error writing file [%s]: %w", filename, err)
	}

	// Compute the SHA384 hash to act as a checksum
	checksum := sha512.Sum384(compressedBytes)

	// Load the existing checksum table
	_, lines, err := r.parseChecksumFile()
	if err != nil {
		return fmt.Errorf("error parsing checkpoint file: %w", err)
	}
	if lines == nil {
		lines = []string{}
	}

	// Add the new record checksum
	checksumLine := fmt.Sprintf("%s  %s", hex.EncodeToString(checksum[:]), filepath.Base(filename))
	lines = append(lines, checksumLine)

	// Sort the lines by their slot
	err = r.sortChecksumEntries(lines)
	if err != nil {
		return fmt.Errorf("error sorting checkpoint file entries: %w", err)
	}

	// Get the number of lines to write
	checkpointRetentionLimit := r.cfg.Smartnode.CheckpointRetentionLimit.Value.(uint64)
	var newLines []string
	if len(lines) > int(checkpointRetentionLimit) {
		numberOfNewLines := int(checkpointRetentionLimit)
		cullCount := len(lines) - numberOfNewLines

		// Remove old lines and delete the corresponding files that shouldn't be retained
		for i := 0; i < cullCount; i++ {
			line := lines[i]

			// Extract the filename
			elems := strings.Split(line, "  ")
			if len(elems) != 2 {
				return fmt.Errorf("error parsing checkpoint line (%s): expected 2 elements, but got %d", line, len(elems))
			}
			filename := elems[1]
			fullFilename := filepath.Join(recordsPath, filename)

			// Delete the file if it exists
			_, err := os.Stat(fullFilename)
			if os.IsNotExist(err) {
				r.log.Printlnf("%s NOTE: tried removing checkpoint file [%s] based on the retention limit, but it didn't exist.", r.logPrefix, filename)
				continue
			}
			err = os.Remove(fullFilename)
			if err != nil {
				return fmt.Errorf("error deleting file [%s]: %w", fullFilename, err)
			}

			r.log.Printlnf("%s Removed checkpoint file [%s] based on the retention limit.", r.logPrefix, filename)
		}

		// Store the rest
		newLines = make([]string, numberOfNewLines)
		for i := cullCount; i <= numberOfNewLines; i++ {
			newLines[i-cullCount] = lines[i]
		}
	} else {
		newLines = lines
	}

	fileContents := strings.Join(newLines, "\n")
	checksumBytes := []byte(fileContents)

	// Save the new file
	checksumFilename := filepath.Join(recordsPath, checksumTableFilename)
	err = os.WriteFile(checksumFilename, checksumBytes, 0644)
	if err != nil {
		return fmt.Errorf("error writing checksum file after culling: %w", err)
	}

	return nil
}

// Load the most recent appropriate rolling record from disk, using the checksum table as an index
func (r *RollingRecordManager) LoadBestRecordFromDisk(startSlot uint64, targetSlot uint64, rewardsInterval uint64) (*RollingRecord, error) {

	// Parse the checksum file
	exists, lines, err := r.parseChecksumFile()
	if err != nil {
		return nil, fmt.Errorf("error parsing checkpoint file: %w", err)
	}
	if !exists {
		// There isn't a checksum file so start over
		r.log.Printlnf("%s Checksum file not found, creating a new record from the start of the interval.", r.logPrefix)
		record := NewRollingRecord(r.log, r.logPrefix, r.bc, startSlot, &r.beaconCfg, rewardsInterval)
		record.LastDutiesSlot = startSlot - 1
		r.Record = record
		r.lastSavedEpoch = record.LastDutiesSlot / r.beaconCfg.SlotsPerEpoch
		return record, nil
	}

	// Iterate over each file, counting backwards from the bottom
	recordsPath := r.cfg.Smartnode.GetRecordsPath()
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]

		// Extract the checksum, filename, and slot number
		checksumString, filename, slot, err := r.parseChecksumEntry(line)
		if err != nil {
			return nil, err
		}

		// Check if the slot was too far into the future
		if slot > targetSlot {
			r.log.Printlnf("%s File [%s] was too far into the future, trying an older one...", r.logPrefix, filename)
			continue
		}

		// Check if it was too far into the past
		if slot < startSlot {
			r.log.Printlnf("%s File [%s] was too old (generated before the target start slot), none of the remaining records can be used.", r.logPrefix, filename)
			break
		}

		// Make sure the checksum parses properly
		checksum, err := hex.DecodeString(checksumString)
		if err != nil {
			return nil, fmt.Errorf("error scanning checkpoint line (%s): checksum (%s) could not be parsed", line, checksumString)
		}

		// Try to load it
		fullFilename := filepath.Join(recordsPath, filename)
		record, err := r.loadRecordFromFile(fullFilename, checksum)
		if err != nil {
			r.log.Printlnf("%s WARNING: error loading record from file [%s]: %s... attempting previous file", r.logPrefix, fullFilename, err.Error())
			continue
		}

		// Check if it was for the proper interval
		if record.RewardsInterval != rewardsInterval {
			r.log.Printlnf("%s File [%s] was for rewards interval %d instead of %d so it cannot be used, trying an earlier checkpoint.", r.logPrefix, filename, record.RewardsInterval, rewardsInterval)
			continue
		}

		epoch := slot / r.beaconCfg.SlotsPerEpoch
		r.log.Printlnf("%s Loaded file [%s] which ended on slot %d (epoch %d) for rewards interval %d.", r.logPrefix, filename, slot, epoch, record.RewardsInterval)
		r.Record = record
		r.lastSavedEpoch = record.LastDutiesSlot / r.beaconCfg.SlotsPerEpoch
		return record, nil

	}

	// If we got here then none of the saved files worked so we have to make a new record
	r.log.Printlnf("%s None of the saved record checkpoint files were eligible for use, creating a new record from the start of the interval.", r.logPrefix)
	record := NewRollingRecord(r.log, r.logPrefix, r.bc, startSlot, &r.beaconCfg, rewardsInterval)
	record.LastDutiesSlot = startSlot - 1
	r.Record = record
	r.lastSavedEpoch = record.LastDutiesSlot / r.beaconCfg.SlotsPerEpoch
	return record, nil

}

// Updates the manager's record to the provided state, retrying upon errors until success
func (r *RollingRecordManager) UpdateRecordToState(state *state.NetworkState, latestFinalizedSlot uint64) error {
	err := r.updateImpl(state, latestFinalizedSlot)
	if err != nil {
		// Revert to the latest saved state
		r.log.Printlnf("%s WARNING: failed to update rolling record to slot %d, block %d: %s", r.logPrefix, state.BeaconSlotNumber, state.ElBlockNumber, err.Error())
		r.log.Printlnf("%s Reverting to the last saved checkpoint to prevent corruption...", r.logPrefix)
		_, err2 := r.LoadBestRecordFromDisk(r.startSlot, latestFinalizedSlot, r.Record.RewardsInterval)
		if err2 != nil {
			return fmt.Errorf("error loading last best checkpoint: %w", err)
		}

		// Try again
		r.log.Printlnf("%s Successfully reverted to the last saved state.", r.logPrefix)
		return err
	}

	return nil
}

// Updates the manager's record to the provided state
func (r *RollingRecordManager) updateImpl(state *state.NetworkState, latestFinalizedSlot uint64) error {
	var err error
	r.log.Printlnf("Updating record to target slot %d...", latestFinalizedSlot)

	// Create a new record if the current one is for the previous rewards interval
	if r.Record.RewardsInterval < state.NetworkDetails.RewardIndex {
		err := r.createNewRecord(state)
		if err != nil {
			return fmt.Errorf("error creating new record: %w", err)
		}
	}

	// Get the state for the target slot
	recordCheckpointInterval := r.cfg.Smartnode.RecordCheckpointInterval.Value.(uint64)
	finalTarget := latestFinalizedSlot
	finalizedState := state
	if finalTarget != state.BeaconSlotNumber {
		finalizedState, err = r.mgr.GetStateForSlot(finalTarget)
		if err != nil {
			return fmt.Errorf("error getting state for latest finalized slot (%d): %w", finalTarget, err)
		}
	}

	// Break the routine into chunks so it can be saved if necessary
	nextStartSlot := r.Record.LastDutiesSlot + 1
	if r.Record.LastDutiesSlot == 0 {
		nextStartSlot = r.startSlot
	}

	nextStartEpoch := nextStartSlot / r.beaconCfg.SlotsPerEpoch
	nextTargetEpoch := nextStartEpoch + recordCheckpointInterval - 1
	if (nextTargetEpoch - r.lastSavedEpoch) > recordCheckpointInterval {
		// Make a stop at the next required checkpoint so it can be saved
		nextTargetEpoch = r.lastSavedEpoch + recordCheckpointInterval
	}
	nextTargetSlot := (nextTargetEpoch+1)*r.beaconCfg.SlotsPerEpoch - 1 // Target is the last slot of the epoch
	if nextTargetSlot > finalTarget {
		nextTargetSlot = finalTarget
		nextTargetEpoch = nextTargetSlot / r.beaconCfg.SlotsPerEpoch
	}
	finalEpoch := finalTarget / r.beaconCfg.SlotsPerEpoch
	totalSlots := float64(finalTarget - nextStartSlot + 1)
	initialSlot := nextStartSlot

	r.log.Printlnf("%s Collecting records from slot %d (epoch %d) to slot %d (epoch %d).", r.logPrefix, nextStartSlot, nextStartEpoch, finalTarget, finalEpoch)
	startTime := time.Now()
	for {
		if nextStartSlot > finalTarget {
			break
		}

		// Update the record to the target state
		err = r.Record.UpdateToSlot(nextTargetSlot, finalizedState)
		if err != nil {
			return fmt.Errorf("error updating rolling record to slot %d, block %d: %w", state.BeaconSlotNumber, state.ElBlockNumber, err)
		}
		slotsProcessed := nextTargetSlot - initialSlot + 1
		r.log.Printf("%s (%.2f%%) Updated from slot %d (epoch %d) to slot %d (epoch %d)... (%s so far) ", r.logPrefix, float64(slotsProcessed)/totalSlots*100.0, nextStartSlot, nextStartEpoch, nextTargetSlot, nextTargetEpoch, time.Since(startTime))

		// Save if required
		if (nextTargetEpoch - r.lastSavedEpoch) >= recordCheckpointInterval {
			err = r.SaveRecordToFile(r.Record)
			if err != nil {
				return fmt.Errorf("error saving record: %w", err)
			}
			r.log.Printlnf("%s Saved record checkpoint.", r.logPrefix)
			r.lastSavedEpoch = nextTargetEpoch
		}

		nextStartSlot = nextTargetSlot + 1
		nextStartEpoch = nextStartSlot / r.beaconCfg.SlotsPerEpoch
		nextTargetEpoch = nextStartEpoch + recordCheckpointInterval - 1
		if (nextTargetEpoch - r.lastSavedEpoch) > recordCheckpointInterval {
			// Make a stop at the next required checkpoint so it can be saved
			nextTargetEpoch = r.lastSavedEpoch + recordCheckpointInterval
		}
		nextTargetSlot = (nextTargetEpoch+1)*r.beaconCfg.SlotsPerEpoch - 1 // Target is the last slot of the epoch
		if nextTargetSlot > finalTarget {
			nextTargetSlot = finalTarget
			nextTargetEpoch = nextTargetSlot / r.beaconCfg.SlotsPerEpoch
		}
	}

	// Log the update
	startEpoch := r.Record.StartSlot / r.beaconCfg.SlotsPerEpoch
	currentEpoch := r.Record.LastDutiesSlot / r.beaconCfg.SlotsPerEpoch
	r.log.Printlnf("%s Record update complete (slot %d-%d, epoch %d-%d).", r.logPrefix, r.Record.StartSlot, r.Record.LastDutiesSlot, startEpoch, currentEpoch)

	return nil
}

// Prepares the record for a rewards interval report
func (r *RollingRecordManager) PrepareRecordForReport(state *state.NetworkState) error {
	rewardsSlot := state.BeaconSlotNumber

	// Check if the current record has gone past the requested slot or if it can be updated / used
	if rewardsSlot < r.Record.LastDutiesSlot {
		r.log.Printlnf("%s Current record has extended too far (need slot %d, but record has processed slot %d)... reverting to a previous checkpoint.", r.logPrefix, rewardsSlot, r.Record.LastDutiesSlot)

		newRecord, err := r.GenerateRecordForState(state)
		if err != nil {
			return fmt.Errorf("error creating record for rewards slot: %w", err)
		}

		r.Record = newRecord
	} else {
		r.log.Printlnf("%s Current record can be used (need slot %d, record has only processed slot %d), updating to target slot.", r.logPrefix, rewardsSlot, r.Record.LastDutiesSlot)
		err := r.UpdateRecordToState(state, rewardsSlot)
		if err != nil {
			return fmt.Errorf("error updating record to rewards slot: %w", err)
		}
	}

	return nil
}

// Get the slot number from a record filename
func (r *RollingRecordManager) getSlotFromFilename(filename string) (uint64, error) {
	matches := r.recordsFilenameRegex.FindStringSubmatch(filename)
	if matches == nil {
		return 0, fmt.Errorf("filename (%s) did not match the expected format", filename)
	}
	slotIndex := r.recordsFilenameRegex.SubexpIndex("slot")
	if slotIndex == -1 {
		return 0, fmt.Errorf("slot number not found in filename (%s)", filename)
	}
	slotString := matches[slotIndex]
	slot, err := strconv.ParseUint(slotString, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("slot (%s) could not be parsed to a number")
	}

	return slot, nil
}

// Load a record from a file, making sure its contents match the provided checksum
func (r *RollingRecordManager) loadRecordFromFile(filename string, expectedChecksum []byte) (*RollingRecord, error) {
	// Read the file
	compressedBytes, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	// Calculate the hash and validate it
	checksum := sha512.Sum384(compressedBytes)
	if !bytes.Equal(expectedChecksum, checksum[:]) {
		expectedString := hex.EncodeToString(expectedChecksum)
		actualString := hex.EncodeToString(checksum[:])
		return nil, fmt.Errorf("checksum mismatch (expected %s, but it was %s)", expectedString, actualString)
	}

	// Decompress it
	bytes, err := r.decompressor.DecodeAll(compressedBytes, []byte{})
	if err != nil {
		return nil, fmt.Errorf("error decompressing data: %w", err)
	}

	// Create a new record from the data
	return DeserializeRollingRecord(r.log, r.logPrefix, r.bc, &r.beaconCfg, bytes)
}

// Get the lines from the checksum file
func (r *RollingRecordManager) parseChecksumFile() (bool, []string, error) {
	// Get the checksum filename
	recordsPath := r.cfg.Smartnode.GetRecordsPath()
	checksumFilename := filepath.Join(recordsPath, checksumTableFilename)

	// Check if the file exists
	_, err := os.Stat(checksumFilename)
	if os.IsNotExist(err) {
		return false, nil, nil
	}

	// Open the checksum file
	checksumTable, err := os.ReadFile(checksumFilename)
	if err != nil {
		return false, nil, fmt.Errorf("error loading checksum table (%s): %w", checksumFilename, err)
	}

	// Parse out each line
	originalLines := strings.Split(string(checksumTable), "\n")

	// Remove empty lines
	lines := make([]string, 0, len(originalLines))
	for _, line := range originalLines {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine != "" {
			lines = append(lines, line)
		}
	}

	return true, lines, nil
}

// Sort the checksum file entries by their slot
func (r *RollingRecordManager) sortChecksumEntries(lines []string) error {
	var sortErr error
	sort.Slice(lines, func(i int, j int) bool {
		_, _, firstSlot, err := r.parseChecksumEntry(lines[i])
		if err != nil && sortErr == nil {
			sortErr = err
			return false
		}

		_, _, secondSlot, err := r.parseChecksumEntry(lines[j])
		if err != nil && sortErr == nil {
			sortErr = err
			return false
		}

		return firstSlot < secondSlot
	})
	return sortErr
}

// Get the checksum, the filename, and the slot number from a checksum entry.
func (r *RollingRecordManager) parseChecksumEntry(line string) (string, string, uint64, error) {
	// Extract the checksum and filename
	elems := strings.Split(line, "  ")
	if len(elems) != 2 {
		return "", "", 0, fmt.Errorf("error parsing checkpoint line (%s): expected 2 elements, but got %d", line, len(elems))
	}
	checksumString := elems[0]
	filename := elems[1]

	// Extract the slot number for this file
	slot, err := r.getSlotFromFilename(filename)
	if err != nil {
		return "", "", 0, fmt.Errorf("error scanning checkpoint line (%s): %w", line, err)
	}

	return checksumString, filename, slot, nil
}

// Creates a new record
func (r *RollingRecordManager) createNewRecord(state *state.NetworkState) error {
	// Get the current interval index
	currentIndexBig, err := rprewards.GetRewardIndex(r.rp, nil)
	if err != nil {
		return fmt.Errorf("error getting rewards index: %w", err)
	}
	currentIndex := currentIndexBig.Uint64()

	// Get the previous RocketRewardsPool addresses
	prevAddresses := r.cfg.Smartnode.GetPreviousRewardsPoolAddresses()

	// Get the last rewards event and starting epoch
	startSlot := uint64(0)
	found, event, err := rprewards.GetRewardsEvent(r.rp, currentIndex-1, prevAddresses, nil)
	if err != nil {
		return fmt.Errorf("error getting event for rewards interval %d: %w", currentIndex-1, err)
	}
	if !found {
		return fmt.Errorf("event for rewards interval %d not found", currentIndex-1)
	}

	// Get the start slot of the current interval
	previousEpoch := event.ConsensusBlock.Uint64() / r.beaconCfg.SlotsPerEpoch
	newEpoch := previousEpoch + 1
	startSlot = newEpoch * r.beaconCfg.SlotsPerEpoch

	// Create a new record for the start slot
	r.log.Printlnf("%s Current record is for interval %d which has passed, creating a new record for interval %d starting on slot %d (epoch %d).", r.logPrefix, r.Record.RewardsInterval, state.NetworkDetails.RewardIndex, startSlot, newEpoch)
	r.Record = NewRollingRecord(r.log, r.logPrefix, r.bc, startSlot, &r.beaconCfg, state.NetworkDetails.RewardIndex)

	return nil
}
