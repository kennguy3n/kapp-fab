import type { Meta, StoryObj } from "@storybook/react";
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
  CardFooter,
} from "./Card";
import { Button } from "./Button";

const meta: Meta<typeof Card> = {
  title: "UI/Card",
  component: Card,
  parameters: { layout: "centered" },
};

export default meta;
type Story = StoryObj<typeof Card>;

export const Basic: Story = {
  render: () => (
    <Card className="w-80">
      <CardHeader>
        <CardTitle>Q3 revenue</CardTitle>
        <CardDescription>vs. Q2 baseline</CardDescription>
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-semibold">$184,231.07</div>
        <div className="text-sm text-success">↑ 12.4% from last quarter</div>
      </CardContent>
    </Card>
  ),
};

export const WithFooter: Story = {
  render: () => (
    <Card className="w-80">
      <CardHeader>
        <CardTitle>Delete tenant?</CardTitle>
        <CardDescription>
          This permanently removes all records and audit history.
        </CardDescription>
      </CardHeader>
      <CardFooter>
        <Button variant="outline">Cancel</Button>
        <Button variant="destructive">Delete</Button>
      </CardFooter>
    </Card>
  ),
};
