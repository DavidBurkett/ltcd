// Copyright (c) 2018 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package psbt

// The Finalizer requires provision of a single PSBT input
// in which all necessary signatures are encoded, and
// uses it to construct valid final sigScript and scriptWitness
// fields.
// NOTE that p2sh (legacy) and p2wsh currently support only
// multisig and no other custom script.

import (
	"bytes"
	"fmt"

	"github.com/ltcsuite/ltcd/btcec/v2/schnorr"
	"github.com/ltcsuite/ltcd/txscript"
	"github.com/ltcsuite/ltcd/wire"
)

// isFinalizableWitnessInput returns true if the target input is a witness UTXO
// that can be finalized.
func isFinalizableWitnessInput(pInput *PInput) bool {
	pkScript := pInput.WitnessUtxo.PkScript

	switch {
	// If this is a native witness output, then we require both
	// the witness script, but not a redeem script.
	case txscript.IsWitnessProgram(pkScript):
		switch {
		case txscript.IsPayToWitnessScriptHash(pkScript):
			if pInput.WitnessScript == nil ||
				pInput.RedeemScript != nil {

				return false
			}

		case txscript.IsPayToTaproot(pkScript):
			if pInput.TaprootKeySpendSig == nil &&
				pInput.TaprootScriptSpendSig == nil {

				return false
			}

			// For each of the script spend signatures we need a
			// corresponding tap script leaf with the control block.
			for _, sig := range pInput.TaprootScriptSpendSig {
				_, err := findLeafScript(pInput, sig.LeafHash)
				if err != nil {
					return false
				}
			}

		default:
			// A P2WKH output on the other hand doesn't need
			// neither a witnessScript or redeemScript.
			if pInput.WitnessScript != nil ||
				pInput.RedeemScript != nil {

				return false
			}
		}

	// For nested P2SH inputs, we verify that a witness script is known.
	case txscript.IsPayToScriptHash(pkScript):
		if pInput.RedeemScript == nil {
			return false
		}

		// If this is a nested P2SH input, then it must also have a
		// witness script, while we don't need one for P2WKH.
		if txscript.IsPayToWitnessScriptHash(pInput.RedeemScript) {
			if pInput.WitnessScript == nil {
				return false
			}
		} else if txscript.IsPayToWitnessPubKeyHash(pInput.RedeemScript) {
			if pInput.WitnessScript != nil {
				return false
			}
		} else {
			// unrecognized type
			return false
		}

	// If this isn't a nested P2SH output or a native witness output, then
	// we can't finalize this input as we don't understand it.
	default:
		return false
	}

	return true
}

// isFinalizableLegacyInput returns true of the passed input a legacy input
// (non-witness) that can be finalized.
func isFinalizableLegacyInput(p *Packet, pInput *PInput, inIndex int) bool {
	// If the input has a witness, then it's invalid.
	if pInput.WitnessScript != nil {
		return false
	}

	// Otherwise, we'll verify that we only have a RedeemScript if the prev
	// output script is P2SH.
	outIndex := p.UnsignedTx.TxIn[inIndex].PreviousOutPoint.Index
	if txscript.IsPayToScriptHash(pInput.NonWitnessUtxo.TxOut[outIndex].PkScript) {
		if pInput.RedeemScript == nil {
			return false
		}
	} else {
		if pInput.RedeemScript != nil {
			return false
		}
	}

	return true
}

// isFinalizable checks whether the structure of the entry for the input of the
// psbt.Packet at index inIndex contains sufficient information to finalize
// this input.
func isFinalizable(p *Packet, inIndex int) bool {
	pInput := p.Inputs[inIndex]

	// The input cannot be finalized without any signatures.
	if pInput.PartialSigs == nil && pInput.TaprootKeySpendSig == nil &&
		pInput.TaprootScriptSpendSig == nil {

		return false
	}

	// For an input to be finalized, we'll one of two possible top-level
	// UTXOs present. Each UTXO type has a distinct set of requirements to
	// be considered finalized.
	switch {

	// A witness input must be either native P2WSH or nested P2SH with all
	// relevant sigScript or witness data populated.
	case pInput.WitnessUtxo != nil:
		if !isFinalizableWitnessInput(&pInput) {
			return false
		}

	case pInput.NonWitnessUtxo != nil:
		if !isFinalizableLegacyInput(p, &pInput, inIndex) {
			return false
		}

	// If neither a known UTXO type isn't present at all, then we'll
	// return false as we need one of them.
	default:
		return false
	}

	return true
}

// MaybeFinalize attempts to finalize the input at index inIndex in the PSBT p,
// returning true with no error if it succeeds, OR if the input has already
// been finalized.
func MaybeFinalize(p *Packet, inIndex int) (bool, error) {
	pInput := p.Inputs[inIndex]
	if pInput.isFinalized() {
		return true, nil
	}

	if !isFinalizable(p, inIndex) {
		return false, ErrNotFinalizable
	}

	if err := Finalize(p, inIndex); err != nil {
		return false, err
	}

	return true, nil
}

// MaybeFinalizeAll attempts to finalize all inputs of the psbt.Packet that are
// not already finalized, and returns an error if it fails to do so.
func MaybeFinalizeAll(p *Packet) error {
	for i := range p.Inputs {
		success, err := MaybeFinalize(p, i)
		if err != nil || !success {
			return err
		}
	}

	// TODO: Finalize MWEB components, which should remove everything except the extractable component fields

	return nil
}

// Finalize assumes that the provided psbt.Packet struct has all partial
// signatures and redeem scripts/witness scripts already prepared for the
// specified input, and so removes all temporary data and replaces them with
// completed sigScript and witness fields, which are stored in key-types 07 and
// 08. The witness/non-witness utxo fields in the inputs (key-types 00 and 01)
// are left intact as they may be needed for validation (?).  If there is any
// invalid or incomplete data, an error is returned.
func Finalize(p *Packet, inIndex int) error {
	pInput := p.Inputs[inIndex]

	// Depending on the UTXO type, we either attempt to finalize it as a
	// witness or legacy UTXO.
	switch {
	case pInput.WitnessUtxo != nil:
		pkScript := pInput.WitnessUtxo.PkScript

		switch {
		case txscript.IsPayToTaproot(pkScript):
			if err := finalizeTaprootInput(p, inIndex); err != nil {
				return err
			}

		default:
			if err := finalizeWitnessInput(p, inIndex); err != nil {
				return err
			}
		}

	case pInput.NonWitnessUtxo != nil:
		if err := finalizeNonWitnessInput(p, inIndex); err != nil {
			return err
		}

	default:
		return ErrInvalidPsbtFormat
	}

	// Before returning we sanity check the PSBT to ensure we don't extract
	// an invalid transaction or produce an invalid intermediate state.
	if err := p.SanityCheck(); err != nil {
		return err
	}

	return nil
}

// finalizeNonWitnessInput attempts to create a PsbtInFinalScriptSig field for
// the input at index inIndex, and removes all other fields except for the UTXO
// field, for an input of type non-witness, or returns an error.
func finalizeNonWitnessInput(p *Packet, inIndex int) error {
	// If this input has already been finalized, then we'll return an error
	// as we can't proceed.
	if p.Inputs[inIndex].isFinalized() {
		return ErrInputAlreadyFinalized
	}

	// Our goal here is to construct a sigScript given the pubkey,
	// signature (keytype 02), of which there might be multiple, and the
	// redeem script field (keytype 04) if present (note, it is not present
	// for p2pkh type inputs).
	var sigScript []byte

	pInput := p.Inputs[inIndex]
	containsRedeemScript := pInput.RedeemScript != nil

	var (
		pubKeys [][]byte
		sigs    [][]byte
	)
	for _, ps := range pInput.PartialSigs {
		pubKeys = append(pubKeys, ps.PubKey)

		sigOK := checkSigHashFlags(ps.Signature, &pInput)
		if !sigOK {
			return ErrInvalidSigHashFlags
		}

		sigs = append(sigs, ps.Signature)
	}

	// We have failed to identify at least 1 (sig, pub) pair in the PSBT,
	// which indicates it was not ready to be finalized. As a result, we
	// can't proceed.
	if len(sigs) < 1 || len(pubKeys) < 1 {
		return ErrNotFinalizable
	}

	// If this input doesn't need a redeem script (P2PKH), then we'll
	// construct a simple sigScript that's just the signature then the
	// pubkey (OP_CHECKSIG).
	var err error
	if !containsRedeemScript {
		// At this point, we should only have a single signature and
		// pubkey.
		if len(sigs) != 1 || len(pubKeys) != 1 {
			return ErrNotFinalizable
		}

		// In this case, our sigScript is just: <sig> <pubkey>.
		builder := txscript.NewScriptBuilder()
		builder.AddData(sigs[0]).AddData(pubKeys[0])
		sigScript, err = builder.Script()
		if err != nil {
			return err
		}
	} else {
		// This is assumed p2sh multisig Given redeemScript and pubKeys
		// we can decide in what order signatures must be appended.
		orderedSigs, err := extractKeyOrderFromScript(
			pInput.RedeemScript, pubKeys, sigs,
		)
		if err != nil {
			return err
		}

		// At this point, we assume that this is a mult-sig input, so
		// we construct our sigScript which looks something like this
		// (mind the extra element for the extra multi-sig pop):
		//  * <nil> <sigs...> <redeemScript>
		//
		// TODO(waxwing): the below is specific to the multisig case.
		builder := txscript.NewScriptBuilder()
		builder.AddOp(txscript.OP_FALSE)
		for _, os := range orderedSigs {
			builder.AddData(os)
		}
		builder.AddData(pInput.RedeemScript)
		sigScript, err = builder.Script()
		if err != nil {
			return err
		}
	}

	// At this point, a sigScript has been constructed.  Remove all fields
	// other than non-witness utxo (00) and finaliscriptsig (07)
	newInput := NewPsbtInput(pInput.NonWitnessUtxo, nil)
	newInput.FinalScriptSig = sigScript

	// Overwrite the entry in the input list at the correct index. Note
	// that this removes all the other entries in the list for this input
	// index.
	p.Inputs[inIndex] = *newInput

	return nil
}

// finalizeWitnessInput attempts to create PsbtInFinalScriptSig field and
// PsbtInFinalScriptWitness field for input at index inIndex, and removes all
// other fields except for the utxo field, for an input of type witness, or
// returns an error.
func finalizeWitnessInput(p *Packet, inIndex int) error {
	// If this input has already been finalized, then we'll return an error
	// as we can't proceed.
	if p.Inputs[inIndex].isFinalized() {
		return ErrInputAlreadyFinalized
	}

	// Depending on the actual output type, we'll either populate a
	// serializedWitness or a witness as well asa sigScript.
	var (
		sigScript         []byte
		serializedWitness []byte
	)

	pInput := p.Inputs[inIndex]

	// First we'll validate and collect the pubkey+sig pairs from the set
	// of partial signatures.
	var (
		pubKeys [][]byte
		sigs    [][]byte
	)
	for _, ps := range pInput.PartialSigs {
		pubKeys = append(pubKeys, ps.PubKey)

		sigOK := checkSigHashFlags(ps.Signature, &pInput)
		if !sigOK {
			return ErrInvalidSigHashFlags

		}

		sigs = append(sigs, ps.Signature)
	}

	// If at this point, we don't have any pubkey+sig pairs, then we bail
	// as we can't proceed.
	if len(sigs) == 0 || len(pubKeys) == 0 {
		return ErrNotFinalizable
	}

	containsRedeemScript := pInput.RedeemScript != nil
	cointainsWitnessScript := pInput.WitnessScript != nil

	// If there's no redeem script, then we assume that this is native
	// segwit input.
	var err error
	if !containsRedeemScript {
		// If we have only a sigley pubkey+sig pair, and no witness
		// script, then we assume this is a P2WKH input.
		if len(pubKeys) == 1 && len(sigs) == 1 &&
			!cointainsWitnessScript {

			serializedWitness, err = writePKHWitness(
				sigs[0], pubKeys[0],
			)
			if err != nil {
				return err
			}
		} else {
			// Otherwise, we must have a witnessScript field, so
			// we'll generate a valid multi-sig witness.
			//
			// NOTE: We tacitly assume multisig.
			//
			// TODO(roasbeef): need to add custom finalize for
			// non-multisig P2WSH outputs (HTLCs, delay outputs,
			// etc).
			if !cointainsWitnessScript {
				return ErrNotFinalizable
			}

			serializedWitness, err = getMultisigScriptWitness(
				pInput.WitnessScript, pubKeys, sigs,
			)
			if err != nil {
				return err
			}
		}
	} else {
		// Otherwise, we assume that this is a p2wsh multi-sig output,
		// which is nested in a p2sh, or a p2wkh nested in a p2sh.
		//
		// In this case, we'll take the redeem script (the witness
		// program in this case), and push it on the stack within the
		// sigScript.
		builder := txscript.NewScriptBuilder()
		builder.AddData(pInput.RedeemScript)
		sigScript, err = builder.Script()
		if err != nil {
			return err
		}

		// If don't have a witness script, then we assume this is a
		// nested p2wkh output.
		if !cointainsWitnessScript {
			// Assumed p2sh-p2wkh Here the witness is just (sig,
			// pub) as for p2pkh case
			if len(sigs) != 1 || len(pubKeys) != 1 {
				return ErrNotFinalizable
			}

			serializedWitness, err = writePKHWitness(
				sigs[0], pubKeys[0],
			)
			if err != nil {
				return err
			}

		} else {
			// Otherwise, we assume that this is a p2wsh multi-sig,
			// so we generate the proper witness.
			serializedWitness, err = getMultisigScriptWitness(
				pInput.WitnessScript, pubKeys, sigs,
			)
			if err != nil {
				return err
			}
		}
	}

	// At this point, a witness has been constructed, and a sigScript (if
	// nested; else it's []). Remove all fields other than witness utxo
	// (01) and finalscriptsig (07), finalscriptwitness (08).
	newInput := NewPsbtInput(nil, pInput.WitnessUtxo)
	if len(sigScript) > 0 {
		newInput.FinalScriptSig = sigScript
	}

	newInput.FinalScriptWitness = serializedWitness

	// Finally, we overwrite the entry in the input list at the correct
	// index.
	p.Inputs[inIndex] = *newInput
	return nil
}

// finalizeTaprootInput attempts to create PsbtInFinalScriptWitness field for
// input at index inIndex, and removes all other fields except for the utxo
// field, for an input of type p2tr, or returns an error.
func finalizeTaprootInput(p *Packet, inIndex int) error {
	// If this input has already been finalized, then we'll return an error
	// as we can't proceed.
	if p.Inputs[inIndex].isFinalized() {
		return ErrInputAlreadyFinalized
	}

	// Any p2tr input will only have a witness script, no sig script.
	var (
		serializedWitness []byte
		err               error
		pInput            = &p.Inputs[inIndex]
	)

	// What spend path did we take?
	switch {
	// Key spend path.
	case len(pInput.TaprootKeySpendSig) > 0:
		sig := pInput.TaprootKeySpendSig

		// Make sure TaprootKeySpendSig is equal to size of signature,
		// if not, we assume that sighash flag was appended to the
		// signature.
		if len(pInput.TaprootKeySpendSig) == schnorr.SignatureSize {
			// Append to the signature if flag is not equal to the
			// default sighash (that can be omitted).
			if pInput.SighashType != txscript.SigHashDefault {
				sigHashType := byte(pInput.SighashType)
				sig = append(sig, sigHashType)
			}
		}
		serializedWitness, err = writeWitness(sig)

	// Script spend path.
	case len(pInput.TaprootScriptSpendSig) > 0:
		var witnessStack wire.TxWitness

		// If there are multiple script spend signatures, we assume they
		// are from multiple signing participants for the same leaf
		// script that uses OP_CHECKSIGADD for multi-sig. Signing
		// multiple possible execution paths at the same time is
		// currently not supported by this library.
		targetLeafHash := pInput.TaprootScriptSpendSig[0].LeafHash
		leafScript, err := findLeafScript(pInput, targetLeafHash)
		if err != nil {
			return fmt.Errorf("control block for script spend " +
				"signature not found")
		}

		// The witness stack will contain all signatures, followed by
		// the script itself and then the control block.
		for idx, scriptSpendSig := range pInput.TaprootScriptSpendSig {
			// Make sure that if there are indeed multiple
			// signatures, they all reference the same leaf hash.
			if !bytes.Equal(scriptSpendSig.LeafHash, targetLeafHash) {
				return fmt.Errorf("script spend signature %d "+
					"references different target leaf "+
					"hash than first signature; only one "+
					"script path is supported", idx)
			}

			sig := append([]byte{}, scriptSpendSig.Signature...)
			if scriptSpendSig.SigHash != txscript.SigHashDefault {
				sig = append(sig, byte(scriptSpendSig.SigHash))
			}
			witnessStack = append(witnessStack, sig)
		}

		// Complete the witness stack with the executed script and the
		// serialized control block.
		witnessStack = append(witnessStack, leafScript.Script)
		witnessStack = append(witnessStack, leafScript.ControlBlock)

		serializedWitness, err = writeWitness(witnessStack...)

	default:
		return ErrInvalidPsbtFormat
	}
	if err != nil {
		return err
	}

	// At this point, a witness has been constructed. Remove all fields
	// other than witness utxo (01) and finalscriptsig (07),
	// finalscriptwitness (08).
	newInput := NewPsbtInput(nil, pInput.WitnessUtxo)
	newInput.FinalScriptWitness = serializedWitness

	// Finally, we overwrite the entry in the input list at the correct
	// index.
	p.Inputs[inIndex] = *newInput
	return nil
}
