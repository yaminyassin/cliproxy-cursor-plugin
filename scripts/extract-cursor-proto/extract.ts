// Extracts the FileDescriptorProto embedded in gajae-code's protoc-gen-es
// generated agent_pb.ts (fileDesc() call) and writes it out as a binary
// FileDescriptorSet that `protoc`/`buf` can consume directly, without
// needing the original .proto source.
//
// Cursor's agent.proto declares some "nested" messages using flat
// top-level naming (e.g. message "ExaFetchRequestResponse_Approved" as
// a sibling of "ExaFetchRequestResponse", referenced from a oneof field
// "approved"). protoc-gen-go independently derives an oneof-case wrapper
// struct name from the parent message + capitalized field name (also
// "ExaFetchRequestResponse_Approved"), so the two collide and the
// generated Go fails to compile with "redeclared in this block".
//
// Message/type *names* are not part of the protobuf binary wire format
// (only field numbers and wire types are), so renaming just the
// colliding top-level messages before Go codegen preserves full wire
// compatibility while producing compilable Go identifiers; JSON field
// names (derived from field declarations, not message names) are
// unaffected.
//
// Collisions are found systematically (every oneof field whose
// protoc-gen-go-derived wrapper name `<Parent>_<CapitalizedField>`
// matches an existing top-level message name), not from a hand-curated
// list, so this stays correct if the upstream schema changes. See
// find-collisions.ts for the standalone detector this logic is shared
// with.
//
// Run with: bun run scripts/extract-cursor-proto/extract.ts
import { create, toBinary } from "@bufbuild/protobuf";
import { FileDescriptorSetSchema } from "@bufbuild/protobuf/wkt";
import { file_agent } from "../../../gajae-code/packages/ai/src/providers/cursor/gen/agent_pb";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const outDir = join(import.meta.dir, "../../internal/cursorpb/proto");
mkdirSync(outDir, { recursive: true });

function capitalizeFieldName(name: string): string {
	return name
		.split("_")
		.map(seg => (seg.length > 0 ? seg[0]!.toUpperCase() + seg.slice(1) : seg))
		.join("");
}

function findOneofWrapperCollisions(proto: typeof file_agent.proto): string[] {
	const messageNames = new Set(proto.messageType.map(m => m.name));
	const collisions = new Set<string>();
	for (const msg of proto.messageType) {
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
				if (messageNames.has(wrapperName)) collisions.add(wrapperName);
			}
		}
	}
	return [...collisions];
}

// file_agent is a GenFile (DescFile); .proto is the underlying
// FileDescriptorProto that protoc-gen-es hydrated from the embedded
// base64 blob at fileDesc() call time. Deep-clone via structuredClone
// so we can safely mutate message names without touching the live
// module-level descriptor object gajae-code itself uses.
const fileDescriptorProto = structuredClone(file_agent.proto);
const pkg = fileDescriptorProto.package;

const collidingNames = findOneofWrapperCollisions(fileDescriptorProto);
const renames = new Map<string, string>(collidingNames.map(name => [name, `${name}Msg`]));

// Rename the top-level message declarations themselves.
let renamedCount = 0;
for (const msg of fileDescriptorProto.messageType) {
	const newName = renames.get(msg.name);
	if (newName !== undefined) {
		msg.name = newName;
		renamedCount++;
	}
}
if (renamedCount !== renames.size) {
	throw new Error(
		`Expected to rename ${renames.size} top-level messages but only renamed ${renamedCount}; descriptor structure changed unexpectedly.`,
	);
}

// Fix every field.type_name reference across the whole file (including
// any true proto nesting) that pointed at an old fully-qualified name,
// so the descriptor stays internally consistent after the rename.
// Fully-qualified names are ".<pkg>.<name>".
function fixFieldRefs(msg: (typeof fileDescriptorProto)["messageType"][number]) {
	for (const field of msg.field) {
		if (!field.typeName) continue;
		const bareName = field.typeName.startsWith(`.${pkg}.`) ? field.typeName.slice(pkg.length + 2) : undefined;
		if (bareName !== undefined) {
			const newBareName = renames.get(bareName);
			if (newBareName !== undefined) field.typeName = `.${pkg}.${newBareName}`;
		}
	}
	for (const nested of msg.nestedType) fixFieldRefs(nested);
}
for (const msg of fileDescriptorProto.messageType) fixFieldRefs(msg);

// Also fix method input/output type references on services, in case any
// of the renamed messages are used directly as RPC request/response types.
for (const service of fileDescriptorProto.service) {
	for (const method of service.method) {
		for (const key of ["inputType", "outputType"] as const) {
			const typeName = method[key];
			if (!typeName) continue;
			const bareName = typeName.startsWith(`.${pkg}.`) ? typeName.slice(pkg.length + 2) : undefined;
			if (bareName !== undefined) {
				const newBareName = renames.get(bareName);
				if (newBareName !== undefined) method[key] = `.${pkg}.${newBareName}`;
			}
		}
	}
}

// Verify no collisions remain after the rename (defense in depth: a
// second full pass over the mutated descriptor should find zero).
const remaining = findOneofWrapperCollisions(fileDescriptorProto);
if (remaining.length > 0) {
	throw new Error(`Rename pass left ${remaining.length} unresolved collisions: ${remaining.join(", ")}`);
}

const descriptorSet = create(FileDescriptorSetSchema, { file: [fileDescriptorProto] });
const bytes = toBinary(FileDescriptorSetSchema, descriptorSet);

const outPath = join(outDir, "agent.protoset");
writeFileSync(outPath, bytes);

console.log(`Wrote FileDescriptorSet (${bytes.length} bytes) to ${outPath}`);
console.log(`Source file: ${fileDescriptorProto.name}, package: ${fileDescriptorProto.package}`);
console.log(`Message count: ${fileDescriptorProto.messageType.length}, service count: ${fileDescriptorProto.service.length}`);
console.log(`Applied ${renames.size} top-level message renames to resolve protoc-gen-go oneof naming collisions:`);
for (const [oldName, newName] of renames) console.log(`  ${oldName} -> ${newName}`);
