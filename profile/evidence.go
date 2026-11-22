package profile

import (
	"sort"

	"rillweave/ingest"
)

// recordRef binds a decoded record to its location. The profile keeps
// these so conflict findings can quote original scalar tokens without
// rescanning during the build phase.
type vertexRecord struct {
	ref ingest.Ref
	raw ingest.RawVertex
}

type arcRecord struct {
	ref ingest.Ref
	raw ingest.RawArc
}

// attachRecords retains decoded inputs for evidence queries. It is
// invoked once per input file during the scan.
func (a *acc) attachRecords(f ingest.InputFile) {
	for i, v := range f.Export.Vertices {
		a.vertexRecords = append(a.vertexRecords, vertexRecord{
			ref: ingest.Ref{File: f.Path, Ordinal: i, ID: v.ID}, raw: v,
		})
	}
	for i, arc := range f.Export.Arcs {
		a.arcRecords = append(a.arcRecords, arcRecord{
			ref: ingest.Ref{File: f.Path, Ordinal: i, ID: arc.ID}, raw: arc,
		})
	}
}

// evidenceScan returns raw-token evidence for one property on one kind,
// bounded and ordered deterministically by input position.
func (a *acc) evidenceScan(vertex bool, kind, property string) []ingest.Evidence {
	var out []ingest.Evidence
	if vertex {
		for _, rec := range a.vertexRecords {
			if rec.raw.Kind != kind {
				continue
			}
			raw, ok := rec.raw.Properties[property]
			if !ok {
				continue
			}
			out = append(out, ingest.EvidenceFor(rec.ref, raw))
			if len(out) == maxEvidence {
				return out
			}
		}
		return out
	}
	for _, rec := range a.arcRecords {
		if rec.raw.Kind != kind {
			continue
		}
		raw, ok := rec.raw.Properties[property]
		if !ok {
			continue
		}
		out = append(out, ingest.EvidenceFor(rec.ref, raw))
		if len(out) == maxEvidence {
			return out
		}
	}
	return out
}

func (a *acc) arcRefs(kind string) []ingest.Ref {
	var refs []ingest.Ref
	for _, rec := range a.arcRecords {
		if rec.raw.Kind == kind {
			refs = append(refs, rec.ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].File != refs[j].File {
			return refs[i].File < refs[j].File
		}
		return refs[i].Ordinal < refs[j].Ordinal
	})
	return refs
}

func (a *acc) rawArc(ref ingest.Ref) ingest.RawArc {
	for _, rec := range a.arcRecords {
		if rec.ref == ref {
			return rec.raw
		}
	}
	return ingest.RawArc{}
}
