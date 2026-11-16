import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva } from "class-variance-authority";

const variants = cva("button", {
  variants: {
    variant: { default: "button-primary", outline: "button-outline" },
  },
  defaultVariants: { variant: "default" },
});

export const Button = React.forwardRef(function Button(
  { className, variant, asChild = false, ...props },
  ref,
) {
  const Component = asChild ? Slot : "button";
  return (
    <Component
      ref={ref}
      className={[variants({ variant }), className].filter(Boolean).join(" ")}
      {...props}
    />
  );
});
