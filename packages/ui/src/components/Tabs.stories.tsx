import type { Meta, StoryObj } from "@storybook/react";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "./Tabs";
import { Card, CardContent, CardHeader, CardTitle } from "./Card";

const meta: Meta = {
  title: "UI/Tabs",
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj;

export const Default: Story = {
  render: () => (
    <Tabs defaultValue="overview" className="w-[500px]">
      <TabsList>
        <TabsTrigger value="overview">Overview</TabsTrigger>
        <TabsTrigger value="activity">Activity</TabsTrigger>
        <TabsTrigger value="settings">Settings</TabsTrigger>
      </TabsList>
      <TabsContent value="overview">
        <Card>
          <CardHeader>
            <CardTitle>Overview</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-fg-muted">
              Summary of the tenant's current quarter.
            </p>
          </CardContent>
        </Card>
      </TabsContent>
      <TabsContent value="activity">
        <Card>
          <CardHeader>
            <CardTitle>Activity</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-fg-muted">
              Audit log entries from the last 7 days.
            </p>
          </CardContent>
        </Card>
      </TabsContent>
      <TabsContent value="settings">
        <Card>
          <CardHeader>
            <CardTitle>Settings</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-fg-muted">Workspace configuration.</p>
          </CardContent>
        </Card>
      </TabsContent>
    </Tabs>
  ),
};
