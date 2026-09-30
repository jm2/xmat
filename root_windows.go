// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build windows

package main

import (
	"errors"
	"fmt"
	"os/user"
	"syscall"
	"unsafe"
)

// Go's os.Mkdir ignores permission bits on Windows: a directory inherits its
// parent's ACL. Under %USERPROFILE% that is private, but on a non-system
// drive (D:\xmat) it usually grants Authenticated Users "Modify". xmat creates
// the root with a protected DACL instead, and checks existing roots.

var (
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	procConvertSDDLToSD      = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetNamedSecurityInfo = advapi32.NewProc("GetNamedSecurityInfoW")
	procGetAce               = advapi32.NewProc("GetAce")
	procLocalFree            = kernel32.NewProc("LocalFree")
)

const (
	sddlRevision1            = 1
	seFileObject             = 1
	ownerSecurityInformation = 0x1
	daclSecurityInformation  = 0x4
	accessAllowedAceType     = 0x0

	sidSystem         = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"
	sidCreatorOwner   = "S-1-3-0"
	sidOwnerRights    = "S-1-3-4"

	// Rights that let a principal change what xmat will later execute.
	writeRights = 0x2 | 0x4 | 0x10 | 0x40 | 0x100 | // write data, append/add subdir, EA, delete child, attributes
		0x10000 | 0x40000 | 0x80000 | // DELETE, WRITE_DAC, WRITE_OWNER
		0x10000000 | 0x40000000 // GENERIC_ALL, GENERIC_WRITE
)

type aclHeader struct {
	AclRevision byte
	Sbz1        byte
	AclSize     uint16
	AceCount    uint16
	Sbz2        uint16
}

type aceHeader struct {
	AceType  byte
	AceFlags byte
	AceSize  uint16
}

type accessAllowedAce struct {
	Header   aceHeader
	Mask     uint32
	SidStart uint32
}

func currentUserSID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Uid, nil // the SID on Windows
}

// mkdirPrivate creates path with a protected DACL: full control for the
// user, SYSTEM and Administrators, inherited by everything below it.
func mkdirPrivate(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	return mkdirSDDL(path, "D:P(A;OICI;FA;;;"+sid+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
}

// mkdirSDDL creates path with the security descriptor described by sddl.
func mkdirSDDL(path, sddlText string) error {
	sddl, err := syscall.UTF16PtrFromString(sddlText)
	if err != nil {
		return err
	}
	var sd unsafe.Pointer
	r, _, e := procConvertSDDLToSD.Call(uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return fmt.Errorf("building security descriptor: %v", e)
	}
	defer procLocalFree.Call(uintptr(sd))
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := syscall.SecurityAttributes{SecurityDescriptor: uintptr(sd)}
	sa.Length = uint32(unsafe.Sizeof(sa))
	return syscall.CreateDirectory(p, &sa)
}

// checkPrivate requires the owner to be the user, SYSTEM or Administrators,
// and no allow ACE (including inherit-only ones, which govern what xmat
// creates below) to grant write access to anyone else.
func checkPrivate(path string) error {
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	trusted := map[string]bool{me: true, sidSystem: true, sidAdministrators: true, sidCreatorOwner: true, sidOwnerRights: true}

	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var owner, dacl, sd unsafe.Pointer
	r, _, _ := procGetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(p)), seFileObject,
		ownerSecurityInformation|daclSecurityInformation,
		uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	if r != 0 {
		return fmt.Errorf("reading its security descriptor: %v", syscall.Errno(r))
	}
	defer procLocalFree.Call(uintptr(sd))

	if owner != nil {
		if s, err := (*syscall.SID)(owner).String(); err == nil && !trusted[s] {
			return fmt.Errorf("it is owned by %s, not by you", accountName((*syscall.SID)(owner), s))
		}
	}
	if dacl == nil {
		return errors.New("it has no access control list, so everyone has full access")
	}
	hdr := (*aclHeader)(dacl)
	for i := 0; i < int(hdr.AceCount); i++ {
		var ace unsafe.Pointer
		if r, _, _ := procGetAce.Call(uintptr(dacl), uintptr(i), uintptr(unsafe.Pointer(&ace))); r == 0 || ace == nil {
			continue
		}
		if (*aceHeader)(ace).AceType != accessAllowedAceType {
			continue
		}
		a := (*accessAllowedAce)(ace)
		if a.Mask&writeRights == 0 {
			continue
		}
		sid := (*syscall.SID)(unsafe.Pointer(&a.SidStart))
		s, err := sid.String()
		if err != nil || trusted[s] {
			continue
		}
		return fmt.Errorf("%s can write to it; use a directory under your profile, or let xmat create a new one", accountName(sid, s))
	}
	return nil
}

func accountName(sid *syscall.SID, fallback string) string {
	if acct, dom, _, err := sid.LookupAccount(""); err == nil {
		if dom != "" {
			return dom + `\` + acct
		}
		return acct
	}
	return fallback
}
