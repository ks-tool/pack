package io.horchestra.pack.sdk;

import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

/**
 * What a field IS and where it GOES. A field without it is the operator's
 * (no default, no const); with kind CONST it is a fact, with DEFAULT a
 * setting whose default is the field's value.
 */
@Retention(RetentionPolicy.RUNTIME)
@Target(ElementType.FIELD)
public @interface Pack {
    enum Kind { OPERATOR, CONST, DEFAULT }

    Kind kind() default Kind.OPERATOR;
    /** JSON property name; the field name when empty (dots in names need this). */
    String json() default "";
    boolean required() default false;
    String title() default "";
    String description() default "";
    /** "" = none; parsed as a number. */
    String minimum() default "";
    String maximum() default "";
    String pattern() default "";
    String[] enumValues() default {};
    /** x-pack-env: the leaf becomes this environment variable. */
    String env() default "";
    /** x-pack-arg: the leaf lands in argv under this flag. */
    String arg() default "";
    String argPrefix() default "";
    boolean argEach() default false;
    boolean argOmitEmpty() default false;

    /** x-pack-mount of a library key ({@link PackRef}): where its layers go. */
    String mount() default "";
    /** composition key: spread replicas over hosts. */
    boolean spread() default false;
    /** composition key: role=key bindings, only where the type is ambiguous. */
    String[] peers() default {};
    /** composition key: extra ordering edges. */
    String[] after() default {};
    /** composition key: resource=quantity lower bounds. */
    String[] minimum() default {};
}
