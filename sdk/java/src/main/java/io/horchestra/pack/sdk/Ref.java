package io.horchestra.pack.sdk;

import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

/**
 * A foreign schema by URI (the platform half). A member of the pack is
 * {@link PackRef}. The field's value, pruned of zeros, is the default layered
 * over the target's.
 */
@Retention(RetentionPolicy.RUNTIME)
@Target(ElementType.FIELD)
public @interface Ref {
    String value();
}
