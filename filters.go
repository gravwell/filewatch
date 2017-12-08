/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package filewatch

import (
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type filter struct {
	bname string //name given to the config file
	loc   string //location we are watching
	mtchs []string
	lh    handler
}

//a unique name that allows multiple IDs pointing at the same file
type FileName struct {
	BaseName string
	FilePath string
}

type FilterManager struct {
	mtx       *sync.Mutex
	filters   []filter
	followers map[FileName]*follower
	states    map[FileName]*int64
	stateFile string
	stateFout *os.File
}

func NewFilterManager(stateFile string) (*FilterManager, error) {
	fout, states, err := initStateFile(stateFile)
	if err != nil {
		return nil, err
	}
	if err := cleanStates(states); err != nil {
		fout.Close()
		return nil, err
	}

	return &FilterManager{
		mtx:       &sync.Mutex{},
		stateFile: stateFile,
		stateFout: fout,
		states:    states,
		followers: map[FileName]*follower{},
	}, nil
}

func (fm *FilterManager) Close() (err error) {
	fm.mtx.Lock()
	defer fm.mtx.Unlock()

	//we have to actually close followers
	for _, v := range fm.followers {
		if lerr := v.Close(); lerr != nil {
			err = appendErr(err, lerr)
		}
	}
	fm.followers = nil

	//just shitcan filters, no need to close anything
	fm.filters = nil

	if err := fm.dumpStates(); err != nil {
		return err
	}
	if err := fm.stateFout.Close(); err != nil {
		return err
	}
	fm.stateFout = nil
	return
}

// Followed returns the current number of following handles
// if a file matches multiple filters, it will be followed multiple
// times.  So this is NOT the number of files, but the number of follows
func (fm *FilterManager) Followed() int {
	fm.mtx.Lock()
	defer fm.mtx.Unlock()
	return len(fm.followers)
}

// Filters returns the current number of installed filters
func (fm *FilterManager) Filters() int {
	fm.mtx.Lock()
	defer fm.mtx.Unlock()
	return len(fm.filters)
}

//dumpStates pushes the current set of states out to a file
//caller MUST HOLD THE LOCK
func (fm *FilterManager) dumpStates() error {
	if fm.stateFout == nil {
		return nil
	}
	n, err := fm.stateFout.Seek(0, 0)
	if err != nil {
		return err
	}
	if n != 0 {
		return ErrFailedSeek
	}
	if err := fm.stateFout.Truncate(0); err != nil {
		return err
	}
	if err := gob.NewEncoder(fm.stateFout).Encode(fm.states); err != nil {
		return err
	}
	return nil
}

func (f *FilterManager) AddFilter(bname, loc string, mtchs []string, lh handler) error {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	fltr := filter{
		bname: bname,
		loc:   filepath.Clean(loc),
		mtchs: mtchs,
		lh:    lh,
	}
	f.filters = append(f.filters, fltr)
	return nil
}

func (f *FilterManager) RemoveFollower(fpath string) error {
	//get file path and base name
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.nolockRemoveFollower(fpath)
}

func (f *FilterManager) nolockRemoveFollower(fpath string) error {
	//check filters
	for _, v := range f.filters {
		//check if we have an active follower
		stid := FileName{
			BaseName: v.bname,
			FilePath: fpath,
		}
		fl, ok := f.followers[stid]
		if ok {
			delete(f.followers, stid)
			delete(f.states, stid)
			if err := fl.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

//walk the directory looking for files, pull the file ID and check if it matches the current file ID
func (f *FilterManager) findFileId(base string, mtchs []string, id FileId) (p string, ok bool, err error) {
	var lid FileId
	//walk the the directory
	err = filepath.Walk(base, func(fpath string, fi os.FileInfo, lerr error) (rerr error) {
		if lerr != nil || fi == nil || ok || !fi.Mode().IsRegular() {
			//is fi is nil then the file isn't there and we can continue
			return
		}

		//check if the file matches any filters
		if f.matchFile(mtchs, filepath.Base(fpath)) {
			//matches the filter, see if it matches the ID
			if lid, rerr = getFileIdFromName(fpath); rerr != nil {
				return
			}
			if lid == id {
				p = fpath
				ok = true
			}
		}
		return
	})
	return
}

// RenameFollower is designed to rename a file that is currently being followed
// We first grab the file id that matches the given fpath
// Then we scan the base directory for ALL files and attempt to match the fileId
// if a match is found, we check if it matches the current filter, if not, we delete the follower
// if it does, we update the name and leave.  If no match is found, we delete the follower
func (f *FilterManager) RenameFollower(fpath string) error {
	//get file path and base name
	stid := FileName{
		FilePath: fpath,
	}

	f.mtx.Lock()
	defer f.mtx.Unlock()

	//find the id for the potentially old filename
	var id FileId
	var hit bool
	for _, flw := range f.followers {
		if flw.FilePath == fpath {
			id = flw.FileId()
			hit = true
		}
	}
	if !hit {
		return nil
	}
	//check filters and their base locations to see if the file showed up anywhere else
	for i, v := range f.filters {
		//check if we have an active follower
		stid.BaseName = v.bname
		flw, ok := f.followers[stid]
		if !ok {
			continue
		}

		//check base directory and pattern match
		p, ok, err := f.findFileId(v.loc, v.mtchs, id)
		if err != nil {
			flw.Close()
			delete(f.states, stid)
			delete(f.followers, stid)
			return err
		}
		if ok {
			//we found it, make sure its not the same damn file name
			if p == fpath {
				return nil
			}
			//different file name and still worth tracking
			if flw.FilterId() != i {
				st, ok := f.states[stid]
				if !ok {
					flw.Close()
					delete(f.followers, stid)
					return errors.New("Failed to find old state")
				}
				delete(f.followers, stid)
				delete(f.states, stid)
				if err := flw.Close(); err != nil {
					return err
				}
				*st = 0
				if err := f.addFollower(v.bname, p, st, i, v.lh); err != nil {
					return err
				}
				return nil
			} else {
				//just update the names
				delete(f.followers, stid)
				flw.FileName = stid
				st, ok := f.states[stid]
				if !ok {
					flw.Close()
					return errors.New("failed to find state on rename")
				}
				stid.FilePath = p
				f.states[stid] = st
				f.followers[stid] = flw
				return nil
			}

		}
	}
	//filename was never found, remove it
	return f.nolockRemoveFollower(fpath)
}

func (f *FilterManager) NewFollower(fpath string) error {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.launchFollowers(fpath, true) // we are deleting the existing state if its there
}

//addFollower gets a new follower, adds it to our list, and launches its routine
//the caller MUST hold the lock
func (f *FilterManager) addFollower(bname, fpath string, si *int64, filterId int, lh handler) error {
	stid := FileName{
		BaseName: bname,
		FilePath: fpath,
	}
	id, err := getFileIdFromName(fpath)
	if err != nil {
		return err
	}
	if flw, ok := f.followers[stid]; ok {
		if flw.FileId() != id {
			//delete the old follower
			delete(f.followers, stid)
			delete(f.states, stid)
			if err := flw.Close(); err != nil {
				return err
			}
		} else {
			return errors.New("duplicate follower")
		}
	}
	fl, err := NewFollower(bname, fpath, si, filterId, lh)
	if err != nil {
		return err
	}
	if err := fl.Start(); err != nil {
		fl.Close()
		return err
	}
	f.followers[stid] = fl
	return nil
}

//look for seek infor for the filename, caller MUST HOLD LOCK
func (f *FilterManager) seekInfo(bname, fpath string) *int64 {
	for k, v := range f.states {
		if k.BaseName == bname && k.FilePath == fpath {
			return v
		}
	}
	return nil
}

func (f *FilterManager) addSeekInfo(bname, fpath string) *int64 {
	stid := FileName{
		BaseName: bname,
		FilePath: fpath,
	}
	si := new(int64)
	f.states[stid] = si
	return si
}

//actually kick off the file follower
func (f *FilterManager) launchFollowers(fpath string, deleteState bool) error {
	//get ID
	id, err := getFileIdFromName(fpath)
	if err != nil {
		return err
	}

	//check if this is just a renaming
	isRename, err := f.checkRename(fpath, id)
	if err != nil {
		return err
	} else if isRename {
		return nil //just a file renaming, continue
	}

	//get base dir
	fname := filepath.Base(fpath)
	fdir := filepath.Dir(fpath)
	var si *int64

	//swing through all filters and launch a follower for each one that matches
	for i, v := range f.filters {
		//check base directory and pattern match
		if v.loc != fdir || !f.matchFile(v.mtchs, fname) {
			continue
		}
		si = nil
		if !deleteState {
			//see if we have state information for this file
			si = f.seekInfo(v.bname, fpath)
		}
		//if not add it
		if si == nil {
			si = f.addSeekInfo(v.bname, fpath)
		}

		if err := f.addFollower(v.bname, fpath, si, i, v.lh); err != nil {
			return err
		}
	}
	return nil
}

//swings through our current set of followers, check if the fileID matches.  If a match is
//found we return true.  This allows us to continue to follow files that are renamed.
//we are given the basename, if a rename is found, search the filters.  If no filter is
//found that matches then we close out the follower and delete the state
//if
//we update the state base name and close out the follower.  If it match
//Caller MUST HOLD THE LOCK
func (f *FilterManager) checkRename(fpath string, id FileId) (isRename bool, err error) {
	var fname string
	var fdir string
	for k, v := range f.followers {
		var removeFollower bool
		if v.FileId() == id {
			fname = filepath.Base(fpath)
			fdir = filepath.Dir(fpath)
			//check if the new name still matches the filter
			filterId := v.FilterId()
			if filterId >= len(f.filters) || filterId < 0 {
				//filter outside of range, delete the follower
				removeFollower = true
			}
			//check the filter glob against the new name
			if f.filters[filterId].loc != fdir || !f.matchFile(f.filters[filterId].mtchs, fname) {
				//this is just a rename, update the fpath in the follower
				delete(f.states, k)
				delete(f.followers, k)
				k.FilePath = fpath
				v.FilePath = fpath
				f.states[k] = v.state
				f.followers[k] = v
				isRename = true
			} else {
				removeFollower = true
			}
			if removeFollower {
				//this is a move away from the current filter, so delete the follower
				//and delete the state
				if err = v.Close(); err != nil {
					return
				}
				delete(f.states, k)
				delete(f.followers, k)
			}
		}
	}
	return
}

func (f *FilterManager) matchFile(mtchs []string, fname string) (matched bool) {
	for _, m := range mtchs {
		if ok, err := filepath.Match(m, fname); err == nil && ok {
			matched = true
			break
		}
	}
	return
}

func (f *FilterManager) LoadFile(fpath string) error {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	return f.launchFollowers(fpath, false)
}

func appendErr(err, nerr error) error {
	if err == nil {
		return nerr
	}
	return fmt.Errorf("%v : %v", err, nerr)
}

func initStateFile(p string) (fout *os.File, states map[FileName]*int64, err error) {
	var fi os.FileInfo
	states = map[FileName]*int64{}
	//attempt to open state file
	fi, err = os.Stat(p)
	if err != nil {
		//ensure error is a "not found" error
		if !os.IsNotExist(err) {
			err = fmt.Errorf("state file path is invalid", err)
			return
		}
		//attempt to create the file and get a handle, states will be empty
		fout, err = os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0660)
		if err != nil {
			return
		}
		return
	}
	//check that is a regular file
	if !fi.Mode().IsRegular() {
		err = ErrInvalidStateFile
		return
	}
	//is a regular file, attempt to open it RW
	fout, err = os.OpenFile(p, os.O_RDWR, 0550) //u+rw and g+rw but no nothing else
	if err != nil {
		err = fmt.Errorf("Failed to open state file RW: %v", err)
		return
	}
	//we have a valid file, attempt to load states if the file isn't empty
	fi, err = fout.Stat()
	if err != nil {
		err = fmt.Errorf("Failed to stat open file: %v", err)
		return
	}
	if fi.Size() > 0 {
		if err = gob.NewDecoder(fout).Decode(&states); err != nil {
			err = fmt.Errorf("Failed to load existing states: %v", err)
			return
		}
	}
	return
}

func cleanStates(states map[FileName]*int64) error {
	for k, v := range states {
		fi, err := os.Stat(k.FilePath)
		if err != nil {
			if os.IsNotExist(err) {
				//file is gone, delete it
				delete(states, k)
			} else {
				return err
			}
		} else {
			//if file shrank, we have to assume this was a truncation, so remove the state
			if v != nil && fi.Size() < *v {
				*v = 0 //reset the size
			}
		}
		//all other cases are just fine, roll
	}
	return nil
}