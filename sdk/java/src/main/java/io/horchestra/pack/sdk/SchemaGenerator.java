package io.horchestra.pack.sdk;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.JsonNodeFactory;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.lang.reflect.Field;
import java.lang.reflect.Modifier;
import java.lang.reflect.ParameterizedType;
import java.lang.reflect.Type;
import java.util.Iterator;
import java.util.List;
import java.util.Map;

/**
 * A component object → the JSON Schema the pack tool reads, with the shape
 * packschema.Reflect gives a Go struct: properties in declaration order,
 * additionalProperties false, const/default from the values, x-pack-*
 * from the annotations, x-pack from {@link Component#root()}, x-pack-compute
 * from {@link Component#compute()}.
 *
 * <p>Property order follows {@code Class.getDeclaredFields()}, which HotSpot
 * returns in declaration order.
 */
public final class SchemaGenerator {
    public static final String SCHEMA = "https://json-schema.org/draft/2020-12/schema";

    private final ObjectMapper mapper = new ObjectMapper()
            .enable(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS);
    private final JsonNodeFactory f = JsonNodeFactory.instance;

    public ObjectNode schema(Component c) {
        ObjectNode s = f.objectNode();
        s.put("$schema", SCHEMA);
        object(c, s);
        Root root = c.root();
        if (root != null) {
            s.set("x-pack", mapper.valueToTree(root));
        }
        for (Map.Entry<String, String> e : c.compute().entrySet()) {
            lookup(s, e.getKey()).put("x-pack-compute", e.getValue());
        }
        return s;
    }

    public String json(Component c) {
        try {
            return mapper.writerWithDefaultPrettyPrinter().writeValueAsString(schema(c)) + "\n";
        } catch (Exception e) {
            throw new IllegalStateException(e);
        }
    }

    private ObjectNode lookup(ObjectNode s, String pointer) {
        ObjectNode node = s;
        for (String seg : pointer.replaceFirst("^/", "").split("/")) {
            JsonNode props = node.get("properties");
            if (props == null || props.get(seg) == null) {
                throw new IllegalArgumentException("x-pack-compute " + pointer + ": no property " + seg);
            }
            node = (ObjectNode) props.get(seg);
        }
        return node;
    }

    /** Fills s with the object schema of value (or of the class when value is null). */
    private void object(Object value, ObjectNode s) {
        Class<?> cls = value instanceof Class<?> ? (Class<?>) value : value.getClass();
        ObjectNode props = f.objectNode();
        ArrayNode required = f.arrayNode();
        for (Field fld : cls.getDeclaredFields()) {
            int mod = fld.getModifiers();
            if (Modifier.isStatic(mod) || Modifier.isTransient(mod) || fld.isSynthetic()) continue;
            fld.setAccessible(true);
            Pack p = fld.getAnnotation(Pack.class);
            String name = p != null && !p.json().isEmpty() ? p.json() : fld.getName();
            Object v;
            try {
                v = value instanceof Class<?> ? null : fld.get(value);
            } catch (IllegalAccessException e) {
                throw new IllegalStateException(e);
            }
            props.set(name, property(fld, v));
            if (p != null && p.required()) required.add(name);
        }
        s.set("properties", props);
        s.put("additionalProperties", false);
        s.put("type", "object");
        if (required.size() > 0) s.set("required", required);
    }

    private ObjectNode property(Field fld, Object value) {
        ObjectNode prop = f.objectNode();
        Pack p = fld.getAnnotation(Pack.class);
        Ref ref = fld.getAnnotation(Ref.class);
        PackRef member = fld.getAnnotation(PackRef.class);
        Class<?> type = fld.getType();
        if (ref != null || member != null) {
            prop.put("$ref", member != null ? "pack://" + member.value() + "#" : ref.value());
            JsonNode d = value == null ? null : prune(mapper.valueToTree(value));
            if (d != null && d.size() > 0) prop.set("default", d);
            if (p != null) {
                if (!p.mount().isEmpty()) prop.put("x-pack-mount", p.mount());
                ObjectNode key = f.objectNode();
                if (p.spread()) key.put("spread", true);
                if (p.peers().length > 0) key.set("peers", pairs(p.peers()));
                if (p.after().length > 0) { ArrayNode a = f.arrayNode(); for (String s : p.after()) a.add(s); key.set("after", a); }
                if (p.minimum().length > 0) key.set("minimum", pairs(p.minimum()));
                if (key.size() > 0) prop.set("x-pack", key);
            }
            return prop;
        }
        if (type == Secret.class) {
            prop.put("type", "string");
            prop.put("format", "password");
        } else if (type == Platform.class) {
            prop.put("$ref", Platform.REF);
            if (value != null) {
                JsonNode d = prune(mapper.valueToTree(value));
                if (d.size() > 0) prop.set("default", d);
            }
            annotate(prop, fld, p);
            return prop;
        } else if (isObject(type)) {
            object(value == null ? type : value, prop);
            annotate(prop, fld, p);
            return prop;
        } else {
            typeOf(fld.getGenericType(), prop);
            Pack.Kind kind = p == null ? Pack.Kind.OPERATOR : p.kind();
            if (kind == Pack.Kind.CONST) prop.set("const", mapper.valueToTree(value));
            else if (kind == Pack.Kind.DEFAULT) prop.set("default", mapper.valueToTree(value));
        }
        annotate(prop, fld, p);
        return prop;
    }

    private ObjectNode pairs(String[] kv) {
        ObjectNode o = f.objectNode();
        for (String s : kv) {
            int i = s.indexOf('=');
            if (i < 0) throw new IllegalArgumentException("expected key=value, got " + s);
            o.put(s.substring(0, i), s.substring(i + 1));
        }
        return o;
    }

    private void annotate(ObjectNode prop, Field fld, Pack p) {
        File file = fld.getAnnotation(File.class);
        if (file != null) {
            ObjectNode xf = f.objectNode();
            xf.put("path", file.path());
            xf.put("format", file.format());
            if (!file.template().isEmpty()) xf.put("template", file.template());
            if (!file.parseAs().isEmpty()) xf.put("parseAs", file.parseAs());
            if (!file.mode().isEmpty()) xf.put("mode", file.mode());
            if (file.perInstance()) xf.put("perInstance", true);
            prop.set("x-pack-file", xf);
        }
        if (p == null) return;
        if (!p.arg().isEmpty()) {
            ObjectNode a = f.objectNode();
            a.put("flag", p.arg());
            if (!p.argPrefix().isEmpty()) a.put("prefix", p.argPrefix());
            if (p.argEach()) a.put("each", true);
            if (p.argOmitEmpty()) a.put("omitEmpty", true);
            prop.set("x-pack-arg", a);
        }
        if (!p.env().isEmpty()) prop.put("x-pack-env", p.env());
        if (!p.title().isEmpty()) prop.put("title", p.title());
        if (!p.description().isEmpty()) prop.put("description", p.description());
        if (!p.minimum().isEmpty()) prop.set("minimum", number(p.minimum()));
        if (!p.maximum().isEmpty()) prop.set("maximum", number(p.maximum()));
        if (!p.pattern().isEmpty()) prop.put("pattern", p.pattern());
        if (p.enumValues().length > 0) {
            ArrayNode e = f.arrayNode();
            for (String s : p.enumValues()) e.add(s);
            prop.set("enum", e);
        }
    }

    private JsonNode number(String s) {
        return s.contains(".") ? f.numberNode(Double.parseDouble(s)) : f.numberNode(Long.parseLong(s));
    }

    private static boolean isObject(Class<?> t) {
        return !(t.isPrimitive() || t == String.class || Number.class.isAssignableFrom(t) || t == Boolean.class
                || List.class.isAssignableFrom(t) || Map.class.isAssignableFrom(t) || t.isArray() || t.isEnum()
                || t == Secret.class || t == Platform.class);
    }

    private void typeOf(Type t, ObjectNode prop) {
        if (t instanceof ParameterizedType pt) {
            Class<?> raw = (Class<?>) pt.getRawType();
            if (List.class.isAssignableFrom(raw)) {
                ObjectNode items = f.objectNode();
                typeOf(pt.getActualTypeArguments()[0], items);
                prop.set("items", items);
                prop.put("type", "array");
                return;
            }
            if (Map.class.isAssignableFrom(raw)) {
                ObjectNode add = f.objectNode();
                typeOf(pt.getActualTypeArguments()[1], add);
                prop.set("additionalProperties", add);
                prop.put("type", "object");
                return;
            }
        }
        Class<?> c = t instanceof Class<?> ? (Class<?>) t : Object.class;
        if (c == int.class || c == long.class || c == short.class || c == Integer.class || c == Long.class || c == Short.class) prop.put("type", "integer");
        else if (c == double.class || c == float.class || c == Double.class || c == Float.class) prop.put("type", "number");
        else if (c == boolean.class || c == Boolean.class) prop.put("type", "boolean");
        else if (c == String.class || c.isEnum()) prop.put("type", "string");
        else if (c.isArray()) { ObjectNode items = f.objectNode(); typeOf(c.getComponentType(), items); prop.set("items", items); prop.put("type", "array"); }
        else if (List.class.isAssignableFrom(c)) { prop.set("items", f.objectNode()); prop.put("type", "array"); }
        else if (Map.class.isAssignableFrom(c)) { prop.set("additionalProperties", f.objectNode()); prop.put("type", "object"); }
    }

    /** Drops zero values — null, "", 0, false, empty — the way Go's prune does for a ref's default. */
    static JsonNode prune(JsonNode n) {
        if (n.isObject()) {
            ObjectNode out = JsonNodeFactory.instance.objectNode();
            Iterator<Map.Entry<String, JsonNode>> it = n.fields();
            while (it.hasNext()) {
                Map.Entry<String, JsonNode> e = it.next();
                JsonNode v = prune(e.getValue());
                if (isZero(v)) continue;
                out.set(e.getKey(), v);
            }
            return out;
        }
        if (n.isArray()) {
            ArrayNode out = JsonNodeFactory.instance.arrayNode();
            for (JsonNode x : n) out.add(prune(x));
            return out;
        }
        return n;
    }

    private static boolean isZero(JsonNode v) {
        return v == null || v.isNull() || (v.isTextual() && v.asText().isEmpty()) || (v.isNumber() && v.asDouble() == 0)
                || (v.isBoolean() && !v.asBoolean()) || ((v.isObject() || v.isArray()) && v.size() == 0);
    }
}
