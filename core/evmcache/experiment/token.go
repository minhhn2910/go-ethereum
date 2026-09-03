// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <https://www.gnu.org/licenses/>.

package experiment

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
)

// transferSelector is keccak256("transfer(address,uint256)")[:4] = 0xa9059cbb.
var transferSelector = []byte{0xa9, 0x05, 0x9c, 0xbb}

// TokenRuntime is a pinned, compiler-independent ERC-20 fragment:
//
//	mapping(address => uint256) balances; // slot 0
//	function transfer(address to, uint256 amount) external returns (bool)
//
// It emits the same SHA3 + SLOAD/SSTORE pattern as a Solidity mapping
// access (keccak256(key ‖ uint256(0))) so hit-rate numbers are comparable
// to a real ERC-20 transfer without pulling in a floating solc.
func TokenRuntime() []byte {
	var code []byte
	emit := func(op vm.OpCode, imm ...byte) {
		code = append(code, byte(op))
		code = append(code, imm...)
	}
	// dispatcher: if selector == transfer, jump; else stop
	emit(vm.PUSH1, 0x00)
	emit(vm.CALLDATALOAD)
	emit(vm.PUSH1, 0xe0)
	emit(vm.SHR)
	emit(vm.PUSH4, transferSelector...)
	emit(vm.EQ)
	emit(vm.PUSH1, 0x00) // placeholder jump dest
	emit(vm.JUMPI)
	emit(vm.STOP)

	transferPC := len(code)
	code[len(code)-3] = byte(transferPC) // patch PUSH1 immediate ([PUSH1][imm][JUMPI][STOP])
	emit(vm.JUMPDEST)

	// stack: to = calldata[4], amount = calldata[36]
	emit(vm.PUSH1, 0x04)
	emit(vm.CALLDATALOAD)
	emit(vm.PUSH1, 0x24)
	emit(vm.CALLDATALOAD)

	// sender slot = keccak256(pad32(caller) ‖ pad32(0))
	emit(vm.CALLER)
	emit(vm.PUSH1, 0x00)
	emit(vm.MSTORE)
	emit(vm.PUSH1, 0x00)
	emit(vm.PUSH1, 0x20)
	emit(vm.MSTORE)
	emit(vm.PUSH1, 0x40)
	emit(vm.PUSH1, 0x00)
	emit(vm.KECCAK256)

	// balances[sender] -= amount
	emit(vm.DUP1)
	emit(vm.SLOAD)
	emit(vm.DUP3)
	emit(vm.SWAP1)
	emit(vm.SUB)
	emit(vm.SWAP1)
	emit(vm.SSTORE)

	// recipient slot = keccak256(pad32(to) ‖ pad32(0))
	emit(vm.DUP2)
	emit(vm.PUSH1, 0x00)
	emit(vm.MSTORE)
	emit(vm.PUSH1, 0x00)
	emit(vm.PUSH1, 0x20)
	emit(vm.MSTORE)
	emit(vm.PUSH1, 0x40)
	emit(vm.PUSH1, 0x00)
	emit(vm.KECCAK256)

	// balances[to] += amount
	emit(vm.DUP1)
	emit(vm.SLOAD)
	emit(vm.DUP3)
	emit(vm.ADD)
	emit(vm.SWAP1)
	emit(vm.SSTORE)

	emit(vm.POP) // amount
	emit(vm.POP) // to
	emit(vm.PUSH1, 0x01)
	emit(vm.PUSH1, 0x00)
	emit(vm.MSTORE)
	emit(vm.PUSH1, 0x20)
	emit(vm.PUSH1, 0x00)
	emit(vm.RETURN)
	return code
}

// MappingSlot is the Solidity storage key for mapping(address=>uint256) at
// slot index: keccak256(pad32(addr) ‖ pad32(index)).
func MappingSlot(addr common.Address, index uint64) common.Hash {
	var buf [64]byte
	copy(buf[12:], addr[:])
	binary.BigEndian.PutUint64(buf[56:], index)
	return crypto.Keccak256Hash(buf[:])
}

// TransferCalldata ABI-encodes transfer(to, amount).
func TransferCalldata(to common.Address, amount uint64) []byte {
	data := make([]byte, 4+32+32)
	copy(data[0:4], transferSelector)
	copy(data[16:36], to[:])
	binary.BigEndian.PutUint64(data[60:68], amount)
	return data
}
