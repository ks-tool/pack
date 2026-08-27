package io.horchestra.pack.sdk;

import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

/** x-pack-file: the subtree (or leaf) becomes a file at path, in the format. */
@Retention(RetentionPolicy.RUNTIME)
@Target(ElementType.FIELD)
public @interface File {
    String path();
    String format();
    String template() default "";
    String parseAs() default "";
    String mode() default "";
    boolean perInstance() default false;
}
