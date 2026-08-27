package io.horchestra.pack.sdk;

import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

/**
 * A member of the pack by name — the property is {@code {"$ref": "pack://<name>#"}}:
 * a library (with {@code @Pack(mount = …)}, where its layers go) or a
 * composition key (with the key's x-pack on {@code @Pack}: spread, peers,
 * after, minimum). The field's value, pruned of zeros, is the default layered
 * over the member's own. The Go tag is {@code pack_ref:"<name>"}.
 */
@Retention(RetentionPolicy.RUNTIME)
@Target(ElementType.FIELD)
public @interface PackRef {
    String value();
}
