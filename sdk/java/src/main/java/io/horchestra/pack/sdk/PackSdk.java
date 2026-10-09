package io.horchestra.pack.sdk;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.stream.Stream;

/** The artifact: pack.yaml, the directory tree oci-packer packs, a generator CLI. */
public final class PackSdk {
    private PackSdk() {}

    public static String schema(Component c) {
        return new SchemaGenerator().json(c);
    }

    public static String packFile(Component c) {
        Meta m = c.meta();
        StringBuilder b = new StringBuilder();
        if (c instanceof Composition comp) {
            b.append("type: application/vnd.horchestra.pack.v1+json\n")
             .append("annotations: {io.horchestra.name: ").append(m.name).append(", io.horchestra.version: \"").append(m.version).append("\"}\n")
             .append("items:\n  - from: file://schema.json\n    type: application/vnd.horchestra.pack.schema.v1+json\n");
            for (Component member : comp.members()) {
                Meta mm = member.meta();
                if (mm.artifact == null || mm.artifact.isEmpty()) {
                    throw new IllegalArgumentException(m.name + ": member " + mm.name + " has no artifact (oci://host/repo)");
                }
                b.append("  - from: ").append(mm.artifact).append(':').append(mm.version).append('\n');
            }
            return b.toString();
        }
        b.append("type: application/vnd.horchestra.component.v1+json\n")
         .append("annotations: {io.horchestra.name: ").append(m.name).append(", io.horchestra.version: \"").append(m.version).append("\"}\n")
         .append("items:\n");
        if (m.image != null && !m.image.isEmpty()) {
            b.append("  - from: ").append(m.image).append("\n    platform: ").append(m.platform).append('\n');
        }
        b.append("  - from: file://schema.json\n    type: application/vnd.horchestra.component.schema.v1+json\n");
        if (m.templates != null && Files.isDirectory(m.templates)) {
            b.append("  - from: dir://templates/\n    type: application/vnd.horchestra.template.v1\n");
        }
        return b.toString();
    }

    private static List<Component> all(Component... items) {
        Map<String, Component> seen = new LinkedHashMap<>();
        for (Component c : items) collect(c, seen);
        List<Component> out = new ArrayList<>(seen.values());
        out.sort((a, b) -> a.meta().name.compareTo(b.meta().name));
        return out;
    }

    private static void collect(Component c, Map<String, Component> seen) {
        if (seen.containsKey(c.meta().name)) return;
        seen.put(c.meta().name, c);
        if (c instanceof Composition comp) for (Component m : comp.members()) collect(m, seen);
    }

    /** Writes dir/name/{schema.json, pack.yaml, templates/} for every item and every member. */
    public static List<Path> generate(Path dir, Component... items) throws IOException {
        List<Path> out = new ArrayList<>();
        for (Component c : all(items)) {
            Meta m = c.meta();
            Path d = dir.resolve(m.name);
            Files.createDirectories(d);
            Files.writeString(d.resolve("schema.json"), schema(c), StandardCharsets.UTF_8);
            Files.writeString(d.resolve("pack.yaml"), packFile(c), StandardCharsets.UTF_8);
            if (m.templates != null && Files.isDirectory(m.templates)) {
                Path dst = d.resolve("templates");
                Files.createDirectories(dst);
                try (Stream<Path> files = Files.walk(m.templates)) {
                    for (Path p : (Iterable<Path>) files::iterator) {
                        if (Files.isRegularFile(p)) {
                            Path target = dst.resolve(m.templates.relativize(p).toString());
                            Files.createDirectories(target.getParent());
                            Files.copy(p, target, java.nio.file.StandardCopyOption.REPLACE_EXISTING);
                        }
                    }
                }
            }
            out.add(d);
        }
        return out;
    }

    /** generate [dir] | schema &lt;name&gt; */
    public static void main(String[] args, Component... items) throws IOException {
        if (args.length == 0) {
            System.err.println("usage: generate [dir] | schema <name>");
            System.exit(2);
        }
        switch (args[0]) {
            case "generate" -> {
                for (Path p : generate(Path.of(args.length > 1 ? args[1] : "."), items)) System.out.println("wrote " + p);
            }
            case "schema" -> {
                if (args.length < 2) { System.err.println("schema <name>"); System.exit(2); }
                for (Component c : all(items)) {
                    if (c.meta().name.equals(args[1])) { System.out.print(schema(c)); return; }
                }
                System.err.println("no component " + args[1]);
                System.exit(1);
            }
            default -> {
                System.err.println("unknown command " + args[0]);
                System.exit(2);
            }
        }
    }
}
