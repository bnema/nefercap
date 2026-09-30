package gui

import "github.com/bnema/nefergui"

// Option slices are built once at init; the view only passes them through.
var (
	rootOpts     = []nefergui.ContainerOption{nefergui.Key("root")}
	titleOpts    = []nefergui.HeadingOption{nefergui.Key("title"), nefergui.Level(1)}
	outputTitle  = sectionOpts("output-title")
	modeTitle    = sectionOpts("mode-title")
	targetTitle  = sectionOpts("target-title")
	outputsGroup = groupOpts("outputs")
	modeGroup    = groupOpts("modes")
	targetGroup  = groupOpts("target")
	recordGroup  = groupOpts("record")
	shotOpts     = []nefergui.ButtonOption{nefergui.Key("mode-screenshot")}
	recOpts      = []nefergui.ButtonOption{nefergui.Key("mode-record")}
	recordNoteOp = noteOpts("record-note")
	warningOpts  = noteOpts("warning")
	statusGood   = []nefergui.ContainerOption{nefergui.Key("status"), nefergui.Class("status")}
	statusBad    = []nefergui.ContainerOption{nefergui.Key("status"), nefergui.Class("status"), nefergui.Class("bad")}
	actionsOpts  = []nefergui.ContainerOption{nefergui.Key("actions"), nefergui.Class("actions")}
	captureOn    = []nefergui.ButtonOption{nefergui.Key("capture"), nefergui.Class("primary")}
	captureOff   = []nefergui.ButtonOption{nefergui.Key("capture"), nefergui.Class("primary"), nefergui.Disabled(true)}
	closeOpts    = []nefergui.ButtonOption{nefergui.Key("close")}

	fileField     = newField("file", "file", "path of the new capture file")
	regionField   = newField("region", "region", "X,Y,WxH (empty = full output)")
	fpsField      = newField("fps", "fps", "1-240")
	sizeField     = newField("size", "resolution", "WxH (empty = source size)")
	durationField = newField("duration", "duration", "seconds or 1m30s; 0 = until Ctrl+C")
)

func sectionOpts(key string) []nefergui.ContainerOption {
	return []nefergui.ContainerOption{nefergui.Key(key), nefergui.Class("section")}
}

func groupOpts(key string) []nefergui.ContainerOption {
	return []nefergui.ContainerOption{nefergui.Key(key), nefergui.Class("group")}
}

func noteOpts(key string) []nefergui.ContainerOption {
	return []nefergui.ContainerOption{nefergui.Key(key), nefergui.Class("note")}
}

// field is a labelled text input row with precomputed options.
type field struct {
	label, key string
	row, name  []nefergui.ContainerOption
	input      []nefergui.EditOption
}

func newField(key, label, placeholder string) *field {
	return &field{
		label: label,
		key:   key,
		row:   []nefergui.ContainerOption{nefergui.Key(key + "-row"), nefergui.Class("field")},
		name:  []nefergui.ContainerOption{nefergui.Key(key + "-label"), nefergui.Class("field-label")},
		input: []nefergui.EditOption{nefergui.Key(key), nefergui.Placeholder(placeholder)},
	}
}

// show declares one row and reports edit and Enter events.
func (f *field) show(parent nefergui.Node, value *string) (changed, submitted bool) {
	row := parent.Row(f.row...)
	row.Text(f.label, f.name...)
	ev := row.Input(f.label, value, f.input...)
	return ev.Changed(), ev.Submitted()
}

// view builds the whole panel from the model. The model is owned by this view
// alone; events mutate it while the frame is built.
func view(f *nefergui.Frame, m *model) {
	root := f.Root(rootOpts...)
	root.Heading("NeferCap", titleOpts...)

	root.Text("output", outputTitle...)
	outs := root.Column(outputsGroup...)
	for i := range m.outputs {
		o := &m.outputs[i]
		outs.Radio(o.label, o.key, &m.output, o.opts...)
	}

	root.Text("capture", modeTitle...)
	modes := root.Column(modeGroup...)
	modes.Radio("screenshot", modeScreenshot, &m.mode, shotOpts...)
	modes.Radio("record", modeRecord, &m.mode, recOpts...)
	m.modeChanged()

	submit := false
	root.Text("target", targetTitle...)
	target := root.Column(targetGroup...)
	changed, entered := fileField.show(target, &m.path)
	if changed {
		m.pathChanged()
	}
	submit = submit || entered
	_, entered = regionField.show(target, &m.region)
	submit = submit || entered

	if m.mode == modeRecord {
		rec := root.Column(recordGroup...)
		for _, e := range [...]struct {
			f *field
			v *string
		}{{fpsField, &m.fps}, {sizeField, &m.size}, {durationField, &m.duration}} {
			_, entered = e.f.show(rec, e.v)
			submit = submit || entered
		}
		rec.Text(recordNote, recordNoteOp...)
	}

	root.Text(warning, warningOpts...)

	text, submittable, ok := m.status()
	if ok {
		root.Text(text, statusGood...)
	} else {
		root.Text(text, statusBad...)
	}

	actions := root.Row(actionsOpts...)
	capture := captureOff
	if submittable {
		capture = captureOn
	}
	if actions.Button("Capture", capture...).Activated() && submittable {
		submit = true
	}
	if actions.Button("Close", closeOpts...).Activated() {
		m.dismiss()
	}
	if submit {
		m.submit()
	}
}
