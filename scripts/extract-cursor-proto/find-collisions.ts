// Systematically finds every protoc-gen-go oneof-wrapper vs message-name
// collision in agent.proto, instead of relying on `go build`'s partial
// "too many errors" cutoff. For every oneof field, protoc-gen-go derives
// a wrapper struct name of `<Parent>_<CapitalizedFieldName>`; if a
// top-level message already has that exact name, compilation collides.
import { file_agent } from "../../../gajae-code/packages/ai/src/providers/cursor/gen/agent_pb";

const messageNames = new Set(file_agent.proto.messageType.map(m => m.name));

function capitalizeFieldName(name: string): string {
	// protoc-gen-go capitalizes each underscore-separated segment and
	// joins them (json_name style -> JsonName), matching Go export rules.
	return name
		.split("_")
		.map(seg => (seg.length > 0 ? seg[0]!.toUpperCase() + seg.slice(1) : seg))
		.join("");
}

const collisions: string[] = [];

for (const msg of file_agent.proto.messageType) {
	// group fields by oneof_index to find which fields are real oneof cases
	const oneofFieldsByIndex = new Map<number, typeof msg.field>();
	for (const field of msg.field) {
		if (field.oneofIndex === undefined) continue;
		const idx = field.oneofIndex;
		if (!oneofFieldsByIndex.has(idx)) oneofFieldsByIndex.set(idx, []);
		oneofFieldsByIndex.get(idx)!.push(field);
	}
	for (const [, fields] of oneofFieldsByIndex) {
		for (const field of fields) {
			const wrapperName = `${msg.name}_${capitalizeFieldName(field.name)}`;
			if (messageNames.has(wrapperName)) {
				collisions.push(wrapperName);
			}
		}
	}
}

console.log(`Found ${collisions.length} collisions:`);
for (const c of collisions.sort()) console.log(`  ${c}`);
