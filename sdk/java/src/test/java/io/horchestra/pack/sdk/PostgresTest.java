package io.horchestra.pack.sdk;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import io.horchestra.pack.sdk.example.Postgres;
import org.junit.jupiter.api.Test;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** The Java description of examples/postgres is the Go one: same schema as a value, same property order. */
class PostgresTest {
    private static final Path GO_SCHEMA = Path.of("..", "..", "examples", "postgres", "schema.json");

    @Test
    void schemaEqualsGo() throws Exception {
        ObjectMapper mapper = new ObjectMapper();
        JsonNode want = mapper.readTree(Files.readString(GO_SCHEMA));
        JsonNode got = new SchemaGenerator().schema(new Postgres());
        assertEquals(want, got, got.toPrettyString());
        assertEquals(orders(want, ""), orders(got, ""));
    }

    @Test
    void packFile() {
        String pf = PackSdk.packFile(new Postgres());
        assertTrue(pf.contains("oci://images/postgres:16.3"));
        assertTrue(pf.contains("io.horchestra.name: postgres"));
    }

    private static Map<String, List<String>> orders(JsonNode node, String path) {
        Map<String, List<String>> out = new java.util.TreeMap<>();
        JsonNode props = node.get("properties");
        if (props != null && props.isObject()) {
            List<String> names = new ArrayList<>();
            Iterator<String> it = props.fieldNames();
            while (it.hasNext()) names.add(it.next());
            out.put(path, names);
            for (String n : names) out.putAll(orders(props.get(n), path + "/" + n));
        }
        return out;
    }
}
