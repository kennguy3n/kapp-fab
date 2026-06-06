import type { Meta, StoryObj } from "@storybook/react";
import { useState } from "react";
import { Stepper } from "./Stepper";
import { Button } from "./Button";

const meta: Meta<typeof Stepper> = {
  title: "UI/Stepper",
  component: Stepper,
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj<typeof Stepper>;

const steps = [
  { label: "Organization", description: "Name & locale" },
  { label: "Chart of accounts", description: "Pick a template" },
  { label: "Invite team", description: "Add users" },
];

export const FirstStep: Story = {
  render: () => (
    <div className="max-w-2xl">
      <Stepper steps={steps} current={0} />
    </div>
  ),
};

export const MiddleStep: Story = {
  render: () => (
    <div className="max-w-2xl">
      <Stepper steps={steps} current={1} />
    </div>
  ),
};

export const Interactive: Story = {
  render: () => {
    const [current, setCurrent] = useState(0);
    return (
      <div className="max-w-2xl space-y-6">
        <Stepper steps={steps} current={current} />
        <div className="flex gap-2">
          <Button
            variant="outline"
            onClick={() => setCurrent((c) => Math.max(0, c - 1))}
            disabled={current === 0}
          >
            Back
          </Button>
          <Button
            onClick={() =>
              setCurrent((c) => Math.min(steps.length - 1, c + 1))
            }
            disabled={current === steps.length - 1}
          >
            Next
          </Button>
        </div>
      </div>
    );
  },
};
