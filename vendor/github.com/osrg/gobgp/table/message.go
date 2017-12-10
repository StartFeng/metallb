// Copyright (C) 2014 Nippon Telegraph and Telephone Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package table

import (
	"bytes"
	"reflect"

	"github.com/osrg/gobgp/packet/bgp"
	log "github.com/sirupsen/logrus"
)

func UpdatePathAttrs2ByteAs(msg *bgp.BGPUpdate) error {
	ps := msg.PathAttributes
	msg.PathAttributes = make([]bgp.PathAttributeInterface, len(ps))
	copy(msg.PathAttributes, ps)
	var asAttr *bgp.PathAttributeAsPath
	idx := 0
	for i, attr := range msg.PathAttributes {
		if a, ok := attr.(*bgp.PathAttributeAsPath); ok {
			asAttr = a
			idx = i
			break
		}
	}

	if asAttr == nil {
		return nil
	}

	as4Params := make([]*bgp.As4PathParam, 0, len(asAttr.Value))
	as2Params := make([]bgp.AsPathParamInterface, 0, len(asAttr.Value))
	mkAs4 := false
	for _, param := range asAttr.Value {
		as4Param := param.(*bgp.As4PathParam)
		as2Path := make([]uint16, 0, len(as4Param.AS))
		for _, as := range as4Param.AS {
			if as > (1<<16)-1 {
				mkAs4 = true
				as2Path = append(as2Path, bgp.AS_TRANS)
			} else {
				as2Path = append(as2Path, uint16(as))
			}
		}
		as2Params = append(as2Params, bgp.NewAsPathParam(as4Param.Type, as2Path))

		// RFC 6793 4.2.2 Generating Updates
		//
		// Whenever the AS path information contains the AS_CONFED_SEQUENCE or
		// AS_CONFED_SET path segment, the NEW BGP speaker MUST exclude such
		// path segments from the AS4_PATH attribute being constructed.
		if as4Param.Type != bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SEQ && as4Param.Type != bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SET {
			as4Params = append(as4Params, as4Param)
		}
	}
	msg.PathAttributes[idx] = bgp.NewPathAttributeAsPath(as2Params)
	if mkAs4 {
		msg.PathAttributes = append(msg.PathAttributes, bgp.NewPathAttributeAs4Path(as4Params))
	}
	return nil
}

func UpdatePathAttrs4ByteAs(msg *bgp.BGPUpdate) error {
	var asAttr *bgp.PathAttributeAsPath
	var as4Attr *bgp.PathAttributeAs4Path
	asAttrPos := 0
	as4AttrPos := 0
	for i, attr := range msg.PathAttributes {
		switch attr.(type) {
		case *bgp.PathAttributeAsPath:
			asAttr = attr.(*bgp.PathAttributeAsPath)
			for j, param := range asAttr.Value {
				as2Param, ok := param.(*bgp.AsPathParam)
				if ok {
					asPath := make([]uint32, 0, len(as2Param.AS))
					for _, as := range as2Param.AS {
						asPath = append(asPath, uint32(as))
					}
					as4Param := bgp.NewAs4PathParam(as2Param.Type, asPath)
					asAttr.Value[j] = as4Param
				}
			}
			asAttrPos = i
			msg.PathAttributes[i] = asAttr
		case *bgp.PathAttributeAs4Path:
			as4AttrPos = i
			as4Attr = attr.(*bgp.PathAttributeAs4Path)
		}
	}

	if as4Attr != nil {
		msg.PathAttributes = append(msg.PathAttributes[:as4AttrPos], msg.PathAttributes[as4AttrPos+1:]...)
	}

	if asAttr == nil || as4Attr == nil {
		return nil
	}

	asLen := 0
	asConfedLen := 0
	asParams := make([]*bgp.As4PathParam, 0, len(asAttr.Value))
	for _, param := range asAttr.Value {
		asLen += param.ASLen()
		p := param.(*bgp.As4PathParam)
		switch p.Type {
		case bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SET:
			asConfedLen += 1
		case bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SEQ:
			asConfedLen += len(p.AS)
		}
		asParams = append(asParams, p)
	}

	as4Len := 0
	as4Params := make([]*bgp.As4PathParam, 0, len(as4Attr.Value))
	if as4Attr != nil {
		for _, p := range as4Attr.Value {
			// RFC 6793 6. Error Handling
			//
			// the path segment types AS_CONFED_SEQUENCE and AS_CONFED_SET [RFC5065]
			// MUST NOT be carried in the AS4_PATH attribute of an UPDATE message.
			// A NEW BGP speaker that receives these path segment types in the AS4_PATH
			// attribute of an UPDATE message from an OLD BGP speaker MUST discard
			// these path segments, adjust the relevant attribute fields accordingly,
			// and continue processing the UPDATE message.
			// This case SHOULD be logged locally for analysis.
			switch p.Type {
			case bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SEQ, bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SET:
				typ := "CONFED_SEQ"
				if p.Type == bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SET {
					typ = "CONFED_SET"
				}
				log.WithFields(log.Fields{
					"Topic": "Table",
				}).Warnf("AS4_PATH contains %s segment %s. ignore", typ, p.String())
				continue
			}
			as4Len += p.ASLen()
			as4Params = append(as4Params, p)
		}
	}

	if asLen+asConfedLen < as4Len {
		log.WithFields(log.Fields{
			"Topic": "Table",
		}).Warn("AS4_PATH is longer than AS_PATH. ignore AS4_PATH")
		return nil
	}

	keepNum := asLen + asConfedLen - as4Len

	newParams := make([]*bgp.As4PathParam, 0, len(asAttr.Value))
	for _, param := range asParams {
		if keepNum-param.ASLen() >= 0 {
			newParams = append(newParams, param)
			keepNum -= param.ASLen()
		} else {
			// only SEQ param reaches here
			param.AS = param.AS[:keepNum]
			newParams = append(newParams, param)
			keepNum = 0
		}

		if keepNum <= 0 {
			break
		}
	}

	for _, param := range as4Params {
		lastParam := newParams[len(newParams)-1]
		if param.Type == lastParam.Type && param.Type == bgp.BGP_ASPATH_ATTR_TYPE_SEQ {
			if len(lastParam.AS)+len(param.AS) > 255 {
				lastParam.AS = append(lastParam.AS, param.AS[:255-len(lastParam.AS)]...)
				param.AS = param.AS[255-len(lastParam.AS):]
				newParams = append(newParams, param)
			} else {
				lastParam.AS = append(lastParam.AS, param.AS...)
			}
		} else {
			newParams = append(newParams, param)
		}
	}

	newIntfParams := make([]bgp.AsPathParamInterface, 0, len(asAttr.Value))
	for _, p := range newParams {
		newIntfParams = append(newIntfParams, p)
	}

	msg.PathAttributes[asAttrPos] = bgp.NewPathAttributeAsPath(newIntfParams)
	return nil
}

func UpdatePathAggregator2ByteAs(msg *bgp.BGPUpdate) {
	as := uint32(0)
	var addr string
	for i, attr := range msg.PathAttributes {
		switch attr.(type) {
		case *bgp.PathAttributeAggregator:
			agg := attr.(*bgp.PathAttributeAggregator)
			addr = agg.Value.Address.String()
			if agg.Value.AS > (1<<16)-1 {
				as = agg.Value.AS
				msg.PathAttributes[i] = bgp.NewPathAttributeAggregator(uint16(bgp.AS_TRANS), addr)
			} else {
				msg.PathAttributes[i] = bgp.NewPathAttributeAggregator(uint16(agg.Value.AS), addr)
			}
		}
	}
	if as != 0 {
		msg.PathAttributes = append(msg.PathAttributes, bgp.NewPathAttributeAs4Aggregator(as, addr))
	}
}

func UpdatePathAggregator4ByteAs(msg *bgp.BGPUpdate) error {
	var aggAttr *bgp.PathAttributeAggregator
	var agg4Attr *bgp.PathAttributeAs4Aggregator
	agg4AttrPos := 0
	for i, attr := range msg.PathAttributes {
		switch attr.(type) {
		case *bgp.PathAttributeAggregator:
			attr := attr.(*bgp.PathAttributeAggregator)
			if attr.Value.Askind == reflect.Uint16 {
				aggAttr = attr
				aggAttr.Value.Askind = reflect.Uint32
			}
		case *bgp.PathAttributeAs4Aggregator:
			agg4Attr = attr.(*bgp.PathAttributeAs4Aggregator)
			agg4AttrPos = i
		}
	}
	if aggAttr == nil && agg4Attr == nil {
		return nil
	}

	if aggAttr == nil && agg4Attr != nil {
		return bgp.NewMessageError(bgp.BGP_ERROR_UPDATE_MESSAGE_ERROR, bgp.BGP_ERROR_SUB_MALFORMED_ATTRIBUTE_LIST, nil, "AS4 AGGREGATOR attribute exists, but AGGREGATOR doesn't")
	}

	if agg4Attr != nil {
		msg.PathAttributes = append(msg.PathAttributes[:agg4AttrPos], msg.PathAttributes[agg4AttrPos+1:]...)
		aggAttr.Value.AS = agg4Attr.Value.AS
	}
	return nil
}

type cage struct {
	attrsBytes []byte
	paths      []*Path
}

func newCage(b []byte, path *Path) *cage {
	return &cage{
		attrsBytes: b,
		paths:      []*Path{path},
	}
}

type packerInterface interface {
	add(*Path)
	pack(options ...*bgp.MarshallingOption) []*bgp.BGPMessage
}

type packer struct {
	eof    bool
	family bgp.RouteFamily
	total  uint32
}

type packerMP struct {
	packer
	paths       []*Path
	withdrawals []*Path
}

func (p *packerMP) add(path *Path) {
	p.packer.total++

	if path.IsEOR() {
		p.packer.eof = true
		return
	}

	if path.IsWithdraw {
		p.withdrawals = append(p.withdrawals, path)
		return
	}

	p.paths = append(p.paths, path)
}

func createMPReachMessage(path *Path) *bgp.BGPMessage {
	oattrs := path.GetPathAttrs()
	attrs := make([]bgp.PathAttributeInterface, 0, len(oattrs))
	for _, a := range oattrs {
		if a.GetType() == bgp.BGP_ATTR_TYPE_MP_REACH_NLRI {
			attrs = append(attrs, bgp.NewPathAttributeMpReachNLRI(path.GetNexthop().String(), []bgp.AddrPrefixInterface{path.GetNlri()}))
		} else {
			attrs = append(attrs, a)
		}
	}
	return bgp.NewBGPUpdateMessage(nil, attrs, nil)
}

func (p *packerMP) pack(options ...*bgp.MarshallingOption) []*bgp.BGPMessage {
	msgs := make([]*bgp.BGPMessage, 0, p.packer.total)

	for _, path := range p.withdrawals {
		nlris := []bgp.AddrPrefixInterface{path.GetNlri()}
		msgs = append(msgs, bgp.NewBGPUpdateMessage(nil, []bgp.PathAttributeInterface{bgp.NewPathAttributeMpUnreachNLRI(nlris)}, nil))
	}

	for _, path := range p.paths {
		msgs = append(msgs, createMPReachMessage(path))
	}

	if p.eof {
		msgs = append(msgs, bgp.NewEndOfRib(p.family))
	}
	return msgs
}

func newPackerMP(f bgp.RouteFamily) *packerMP {
	return &packerMP{
		packer: packer{
			family: f,
		},
		withdrawals: make([]*Path, 0),
		paths:       make([]*Path, 0),
	}
}

type packerV4 struct {
	packer
	hashmap     map[uint32][]*cage
	mpPaths     []*Path
	withdrawals []*Path
}

func (p *packerV4) add(path *Path) {
	p.packer.total++

	if path.IsEOR() {
		p.packer.eof = true
		return
	}

	if path.IsWithdraw {
		p.withdrawals = append(p.withdrawals, path)
		return
	}

	if path.GetNexthop().To4() == nil {
		// RFC 5549
		p.mpPaths = append(p.mpPaths, path)
		return
	}

	key := path.GetHash()
	attrsB := bytes.NewBuffer(make([]byte, 0))
	for _, v := range path.GetPathAttrs() {
		b, _ := v.Serialize()
		attrsB.Write(b)
	}

	if cages, y := p.hashmap[key]; y {
		added := false
		for _, c := range cages {
			if bytes.Compare(c.attrsBytes, attrsB.Bytes()) == 0 {
				c.paths = append(c.paths, path)
				added = true
				break
			}
		}
		if !added {
			p.hashmap[key] = append(p.hashmap[key], newCage(attrsB.Bytes(), path))
		}
	} else {
		p.hashmap[key] = []*cage{newCage(attrsB.Bytes(), path)}
	}
}

func (p *packerV4) pack(options ...*bgp.MarshallingOption) []*bgp.BGPMessage {
	split := func(max int, paths []*Path) ([]*bgp.IPAddrPrefix, []*Path) {
		nlris := make([]*bgp.IPAddrPrefix, 0, max)
		i := 0
		if max > len(paths) {
			max = len(paths)
		}
		for ; i < max; i++ {
			nlris = append(nlris, paths[i].GetNlri().(*bgp.IPAddrPrefix))
		}
		return nlris, paths[i:]
	}
	addpathNLRILen := 0
	if bgp.IsAddPathEnabled(false, p.packer.family, options) {
		addpathNLRILen = 4
	}
	// Header + Update (WithdrawnRoutesLen +
	// TotalPathAttributeLen + attributes + maxlen of NLRI).
	// the max size of NLRI is 5bytes (plus 4bytes with addpath enabled)
	maxNLRIs := func(attrsLen int) int {
		return (bgp.BGP_MAX_MESSAGE_LENGTH - (19 + 2 + 2 + attrsLen)) / (5 + addpathNLRILen)
	}

	loop := func(attrsLen int, paths []*Path, cb func([]*bgp.IPAddrPrefix)) {
		max := maxNLRIs(attrsLen)
		var nlris []*bgp.IPAddrPrefix
		for {
			nlris, paths = split(max, paths)
			if len(nlris) == 0 {
				break
			}
			cb(nlris)
		}
	}

	msgs := make([]*bgp.BGPMessage, 0, p.packer.total)

	loop(0, p.withdrawals, func(nlris []*bgp.IPAddrPrefix) {
		msgs = append(msgs, bgp.NewBGPUpdateMessage(nlris, nil, nil))
	})

	for _, cages := range p.hashmap {
		for _, c := range cages {
			paths := c.paths

			attrs := paths[0].GetPathAttrs()
			attrsLen := 0
			for _, a := range attrs {
				attrsLen += a.Len()
			}

			loop(attrsLen, paths, func(nlris []*bgp.IPAddrPrefix) {
				msgs = append(msgs, bgp.NewBGPUpdateMessage(nil, attrs, nlris))
			})
		}
	}

	for _, path := range p.mpPaths {
		msgs = append(msgs, createMPReachMessage(path))
	}

	if p.eof {
		msgs = append(msgs, bgp.NewEndOfRib(p.family))
	}
	return msgs
}

func newPackerV4(f bgp.RouteFamily) *packerV4 {
	return &packerV4{
		packer: packer{
			family: f,
		},
		hashmap:     make(map[uint32][]*cage),
		withdrawals: make([]*Path, 0),
		mpPaths:     make([]*Path, 0),
	}
}

func newPacker(f bgp.RouteFamily) packerInterface {
	switch f {
	case bgp.RF_IPv4_UC:
		return newPackerV4(bgp.RF_IPv4_UC)
	default:
		return newPackerMP(f)
	}
}

func CreateUpdateMsgFromPaths(pathList []*Path, options ...*bgp.MarshallingOption) []*bgp.BGPMessage {
	msgs := make([]*bgp.BGPMessage, 0, len(pathList))

	m := make(map[bgp.RouteFamily]packerInterface)
	for _, path := range pathList {
		f := path.GetRouteFamily()
		if _, y := m[f]; !y {
			m[f] = newPacker(f)
		}
		m[f].add(path)
	}

	for _, p := range m {
		msgs = append(msgs, p.pack(options...)...)
	}
	return msgs
}